package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
	"github.com/zzzinho/on-prem-node-provisioner/internal/scheduler"
)

// scaleUpRequeue is how often a still-pending pod is re-checked while a wake is
// in flight or after we wake a Machine, so we keep watching until the pod binds
// (at which point it is no longer Unschedulable and the predicate drops it).
const scaleUpRequeue = 30 * time.Second

// noFitRequeue is the slower re-check for a pod that nothing currently fits, so
// a Machine added or relabelled later is reconsidered without a Warning storm.
const noFitRequeue = 60 * time.Second

// minCooldownRequeue floors the requeue we compute from a pool's cooldown
// expiry, so a near-zero remaining interval still yields a real wait rather than
// a hot loop racing the clock's resolution.
const minCooldownRequeue = time.Second

// reasonScaleUp is the Event reason emitted when a Machine is woken to host a
// pending pod.
const reasonScaleUp = "ScaleUp"

// reasonScaleUpBlocked is the Event reason emitted on a Pod when the only
// fitting Off Machine sits in a pool already at its maxNodes cap.
const reasonScaleUpBlocked = "ScaleUpBlocked"

// ScaleUpReconciler reacts to unschedulable pending Pods by waking a powered-off
// Machine that could host them. It does selection only: it sets the
// onp.io/wake-now annotation on the chosen Machine and lets MachineReconciler
// run the actual PowerOn -> Booting -> Ready path, so manual and automatic wake
// share one code path.
type ScaleUpReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Clock reads the current time for cooldown math; injected so tests can drive
	// it with a fake clock. A PassiveClock is enough — cooldown is evaluated each
	// reconcile, not scheduled.
	Clock clock.PassiveClock
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=onp.io,resources=nodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=onp.io,resources=nodepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=onp.io,resources=machines,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile picks a powered-off Machine to wake for one unschedulable Pod. It is
// driven by the Pod, re-verifies candidacy (the watch predicate pre-filters, but
// state may have changed), and is idempotent: if a fitting wake is already in
// flight it waits rather than waking another node.
func (r *ScaleUpReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		// Not found means the Pod was deleted between enqueue and now.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pod.DeletionTimestamp.IsZero() {
		// Being deleted; it will not need a node.
		return ctrl.Result{}, nil
	}
	if !isScaleUpCandidate(&pod) {
		// State changed since the predicate admitted it (bound, scheduled, no
		// longer Pending). Nothing to do.
		return ctrl.Result{}, nil
	}

	var pools v1alpha1.NodePoolList
	if err := r.List(ctx, &pools); err != nil {
		return ctrl.Result{}, fmt.Errorf("list nodepools: %w", err)
	}

	// Walk every pool's members, fit-checking each against a synthetic Node that
	// describes how the Machine will look once Ready. We track, across all pools:
	// Off candidates we could wake (each carrying its pool so we can stamp that
	// pool's cooldown on wake), and whether a fitting wake is already in flight (a
	// wake whose success will satisfy this pod). The two per-pool guards — maxNodes
	// and cooldown.scaleUp — gate which Off members become candidates.
	var (
		candidates   []wakeCandidate
		wakeInFlight bool
		// maxedPool names a pool whose only obstacle to hosting this pod is its
		// maxNodes cap, so we can warn precisely (empty when none applies).
		maxedPool string
		// coolingUntil is the soonest a cooling-down pool's interval lifts, so the
		// pod can be requeued exactly then rather than on the slow noFit cadence.
		coolingUntil time.Time
	)
	for i := range pools.Items {
		pool := &pools.Items[i]
		selector, err := metav1.LabelSelectorAsSelector(&pool.Spec.MachineSelector)
		if err != nil {
			// A malformed selector is an operator error that will not fix itself;
			// skip this pool rather than fail the whole reconcile.
			logger.Error(err, "skip pool with bad machineSelector", "pool", pool.Name)
			continue
		}

		var machines v1alpha1.MachineList
		if err := r.List(ctx, &machines, client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return ctrl.Result{}, fmt.Errorf("list machines for pool %q: %w", pool.Name, err)
		}

		// Per-pool guards, computed once over the membership: a pool at its
		// maxNodes cap cannot wake any member; a pool still inside its
		// cooldown.scaleUp window cannot wake another member until it lifts.
		maxed := poolAtCap(pool, machines.Items)
		poolCoolingUntil := r.coolingUntil(pool)

		for j := range machines.Items {
			m := &machines.Items[j]
			node := nodeForMachine(m, pool)
			if !scheduler.Fit(&pod, node).Fits {
				continue
			}
			if isWaking(m) {
				wakeInFlight = true
				continue
			}
			if m.Status.State != v1alpha1.MachineStateOff {
				continue
			}
			// A fitting Off member, but the pool's guards may forbid waking it.
			if maxed {
				maxedPool = pool.Name
				continue
			}
			if !poolCoolingUntil.IsZero() {
				if coolingUntil.IsZero() || poolCoolingUntil.Before(coolingUntil) {
					coolingUntil = poolCoolingUntil
				}
				continue
			}
			candidates = append(candidates, wakeCandidate{machine: m, pool: pool})
		}
	}

	// In-flight guard first: a fitting Machine is already booting (or already has
	// wake-now set), so waking another would over-provision. M3.2 is intentionally
	// one-pod-at-a-time; batching / bin-packing many pending pods is Phase 2.
	if wakeInFlight {
		logger.V(1).Info("wake already in flight for pod; waiting", "pod", req.NamespacedName)
		return ctrl.Result{RequeueAfter: scaleUpRequeue}, nil
	}

	if len(candidates) > 0 {
		// Best-fit across every pool's candidates: wake the smallest Machine that
		// still fits so a tiny pod does not wake a huge node. Order by (milliCPU
		// asc, memory bytes asc, name asc) for a deterministic winner.
		winner := smallestCandidate(candidates)
		// Stamp the winning pool's cooldown anchor before waking, so a wake that
		// then fails to patch the Machine has not silently skipped rate-limiting.
		if err := r.stampScaleUp(ctx, winner.pool); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.requestWake(ctx, winner.machine); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(winner.machine, corev1.EventTypeNormal, reasonScaleUp,
			"waking machine %q to schedule pod %s/%s", winner.machine.Name, pod.Namespace, pod.Name)
		r.Recorder.Eventf(&pod, corev1.EventTypeNormal, reasonScaleUp,
			"waking machine %q to host pod", winner.machine.Name)

		// Keep checking until the pod schedules; once it binds it is no longer
		// Unschedulable and the predicate stops enqueueing it.
		return ctrl.Result{RequeueAfter: scaleUpRequeue}, nil
	}

	if !coolingUntil.IsZero() {
		// A fitting Off member exists but its pool is cooling down. Requeue exactly
		// when the soonest cooldown lifts (clamped to a small floor, never
		// negative) so we wake promptly instead of waiting out the slow noFit re-check.
		wait := coolingUntil.Sub(r.Clock.Now())
		if wait < minCooldownRequeue {
			wait = minCooldownRequeue
		}
		logger.V(1).Info("fitting off machine blocked by scale-up cooldown; waiting", "pod", req.NamespacedName, "after", wait)
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	if maxedPool != "" {
		// The only fitting Off member sits in a pool at its maxNodes cap. Warn once
		// on the Pod — the recorder aggregates repeats, so the slow requeue below
		// will not storm — and re-check on the slow cadence in case the cap or
		// membership changes.
		r.Recorder.Eventf(&pod, corev1.EventTypeWarning, reasonScaleUpBlocked,
			"pool %q is at its maxNodes cap; not waking a node for this pod", maxedPool)
		return ctrl.Result{RequeueAfter: noFitRequeue}, nil
	}

	// Nothing in any pool can host this pod right now. Re-check on a slower cadence
	// so a Machine added or relabelled later is reconsidered; no per-reconcile
	// Warning to avoid an Event storm.
	logger.V(1).Info("no fitting off machine for pod", "pod", req.NamespacedName)
	return ctrl.Result{RequeueAfter: noFitRequeue}, nil
}

// wakeCandidate is a fitting Off Machine paired with the pool it belongs to, so
// the selection can read that pool's guards and stamp its cooldown on wake.
type wakeCandidate struct {
	machine *v1alpha1.Machine
	pool    *v1alpha1.NodePool
}

// stampScaleUp records that ONP just woke a Machine in this pool by writing
// status.LastScaleUpTime, the anchor for cooldown.scaleUp. It uses a MergeFrom
// status patch so it sends only that one field: NodePoolReconciler also patches
// this status (TotalMachines/ReadyMachines/Conditions) with a MergeFrom patch,
// and two field-scoped merge patches do not clobber each other.
func (r *ScaleUpReconciler) stampScaleUp(ctx context.Context, pool *v1alpha1.NodePool) error {
	orig := pool.DeepCopy()
	now := metav1.NewTime(r.Clock.Now())
	pool.Status.LastScaleUpTime = &now
	if err := r.Status().Patch(ctx, pool, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("stamp scale-up time on nodepool %q: %w", pool.Name, err)
	}
	return nil
}

// requestWake sets the one-shot wake-now annotation via a merge patch so it does
// not clobber concurrent spec edits, mirroring removeWakeAnnotation.
func (r *ScaleUpReconciler) requestWake(ctx context.Context, m *v1alpha1.Machine) error {
	patch := client.MergeFrom(m.DeepCopy())
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations[v1alpha1.AnnotationWakeNow] = v1alpha1.AnnotationWakeNowValue
	if err := r.Patch(ctx, m, patch); err != nil {
		return fmt.Errorf("set wake-now annotation on machine %q: %w", m.Name, err)
	}
	return nil
}

// nodeForMachine builds the synthetic Node a Machine will present once Ready: its
// declared Capacity becomes allocatable, its Node labels are the pool Template
// labels overlaid by the Machine's own Labels (Machine wins on conflict — the
// per-node label is the more specific intent), and the pool Template taints are
// applied. scheduler.Fit reads only Allocatable, Labels and Taints, so that is
// all this assembles.
func nodeForMachine(m *v1alpha1.Machine, pool *v1alpha1.NodePool) *corev1.Node {
	labels := make(map[string]string, len(pool.Spec.Template.Labels)+len(m.Spec.Labels))
	for k, v := range pool.Spec.Template.Labels {
		labels[k] = v
	}
	for k, v := range m.Spec.Labels {
		labels[k] = v
	}

	var taints []corev1.Taint
	if len(pool.Spec.Template.Taints) > 0 {
		taints = append(taints, pool.Spec.Template.Taints...)
	}

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   m.Spec.NodeName,
			Labels: labels,
		},
		Spec: corev1.NodeSpec{Taints: taints},
		Status: corev1.NodeStatus{
			Allocatable: m.Spec.Capacity,
		},
	}
}

// smallestCandidate returns the candidate whose Machine is smallest by capacity,
// ordered by milliCPU, then memory bytes, then name for determinism. The slice
// is non-empty.
func smallestCandidate(candidates []wakeCandidate) wakeCandidate {
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].machine, candidates[j].machine
		if c := cpuMilli(a).Cmp(*cpuMilli(b)); c != 0 {
			return c < 0
		}
		if c := memBytes(a).Cmp(*memBytes(b)); c != 0 {
			return c < 0
		}
		return a.Name < b.Name
	})
	return candidates[0]
}

// poolAtCap reports whether waking another member of this pool would exceed its
// maxNodes. A nil MaxNodes is unbounded. The count is the pool's active members:
// those powered on, transitioning while on, or about to power on (see isActive).
func poolAtCap(pool *v1alpha1.NodePool, members []v1alpha1.Machine) bool {
	if pool.Spec.MaxNodes == nil {
		return false
	}
	var active int32
	for i := range members {
		if isActive(&members[i]) {
			active++
		}
	}
	return active >= *pool.Spec.MaxNodes
}

// isActive reports whether a Machine counts against its pool's maxNodes cap: it
// is powered on or transitioning while on (Ready, Booting, Draining,
// ShuttingDown), or it is Off but already triggered to wake. Draining and
// ShuttingDown do not occur until M4, but counting them now is forward-correct.
func isActive(m *v1alpha1.Machine) bool {
	switch m.Status.State {
	case v1alpha1.MachineStateReady,
		v1alpha1.MachineStateBooting,
		v1alpha1.MachineStateDraining,
		v1alpha1.MachineStateShuttingDown:
		return true
	case v1alpha1.MachineStateOff:
		return wakeRequested(m)
	default:
		return false
	}
}

// coolingUntil returns the instant a pool's scale-up cooldown lifts, or the zero
// time if the pool is not rate-limited right now (no cooldown configured, no
// prior scale-up, or the interval has already elapsed).
func (r *ScaleUpReconciler) coolingUntil(pool *v1alpha1.NodePool) time.Time {
	cd := pool.Spec.Cooldown.ScaleUp
	last := pool.Status.LastScaleUpTime
	if cd == nil || last == nil {
		return time.Time{}
	}
	expiry := last.Time.Add(cd.Duration)
	if !r.Clock.Now().Before(expiry) {
		return time.Time{}
	}
	return expiry
}

// cpuMilli returns the Machine's declared CPU capacity; absent reads as zero.
func cpuMilli(m *v1alpha1.Machine) *resource.Quantity {
	q := m.Spec.Capacity[corev1.ResourceCPU]
	return &q
}

// memBytes returns the Machine's declared memory capacity; absent reads as zero.
func memBytes(m *v1alpha1.Machine) *resource.Quantity {
	q := m.Spec.Capacity[corev1.ResourceMemory]
	return &q
}

// isWaking reports whether a Machine already has a wake in progress that will
// satisfy a pending pod: it is Booting, or it is Off with the wake-now trigger
// already set (the MachineReconciler has not yet advanced it to Booting).
func isWaking(m *v1alpha1.Machine) bool {
	if m.Status.State == v1alpha1.MachineStateBooting {
		return true
	}
	return m.Status.State == v1alpha1.MachineStateOff && wakeRequested(m)
}

// isScaleUpCandidate reports whether a Pod needs a node woken for it: it is
// unbound, Pending, and the scheduler marked it PodScheduled=False with reason
// Unschedulable.
func isScaleUpCandidate(pod *corev1.Pod) bool {
	if pod.Spec.NodeName != "" || pod.Status.Phase != corev1.PodPending {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled {
			return c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable
		}
	}
	return false
}

// SetupWithManager wires the reconciler to watch only unschedulable pending
// Pods, so it does not reconcile every Pod in the cluster. RequeueAfter drives
// the re-checks; no Machine watch is needed for M3.2.
func (r *ScaleUpReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}, builder.WithPredicates(unschedulablePodPredicate())).
		Named("scaleup").
		Complete(r)
}

// unschedulablePodPredicate admits only Pods that currently need a node woken,
// on both Create and Update, so the work queue stays tight.
func unschedulablePodPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			pod, ok := e.Object.(*corev1.Pod)
			return ok && isScaleUpCandidate(pod)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			pod, ok := e.ObjectNew.(*corev1.Pod)
			return ok && isScaleUpCandidate(pod)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
