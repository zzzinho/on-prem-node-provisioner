package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	resourcehelper "k8s.io/component-helpers/resource"
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

// readySettle is how long after reaching Ready a fitting Machine still counts as
// a wake in flight. A just-woken node often cannot take its pod yet — the GPU
// device plugin registers after the kubelet goes Ready — and without the window
// the pod's next reconcile would wake another Machine for it.
//
// ponytail: a fixed window. A pod blocked for longer by something fit does not
// model wakes the next Machine after it; the fit-fidelity work (pod volumes, real
// Node labels and taints) is the upgrade path.
const readySettle = 2 * time.Minute

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
	// APIReader reads straight from the API server. A pending pod's claims and
	// their volumes are read through it with get alone, so the controller needs no
	// cluster-wide list/watch on PersistentVolumes or claims. main.go wires
	// mgr.GetAPIReader().
	APIReader client.Reader
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=onp.io,resources=nodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=onp.io,resources=nodepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=onp.io,resources=machines,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims;persistentvolumes,verbs=get

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
	memberships, err := poolMemberships(ctx, r.Client, pools.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	volumeAffinities, err := podVolumeAffinities(ctx, r.APIReader, &pod)
	if err != nil {
		return ctrl.Result{}, err
	}
	onVolumeNode := scheduler.VolumeNodeAffinity(volumeAffinities)

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
		maxed, err := r.poolAtCap(ctx, pool, machines.Items)
		if err != nil {
			return ctrl.Result{}, err
		}
		poolCoolingUntil := r.coolingUntil(pool)

		for j := range machines.Items {
			m := &machines.Items[j]
			real, err := backingNode(ctx, r.Client, m.Spec.NodeName)
			if err != nil {
				return ctrl.Result{}, err
			}
			bound, err := boundRequests(ctx, r.Client, m.Spec.NodeName)
			if err != nil {
				return ctrl.Result{}, err
			}
			node := nodeForMachine(m, pool, real, bound)
			if !scheduler.Fit(&pod, node, onVolumeNode).Fits {
				continue
			}
			if isWaking(m) || r.settling(m) {
				wakeInFlight = true
				continue
			}
			// Only an Off Machine not yet asked to wake is a candidate; one whose
			// power-on keeps failing already carries the request.
			if m.Status.State != v1alpha1.MachineStateOff || wakeRequested(m) {
				continue
			}
			// A Machine in more than one pool has no single policy to wake it under
			// (whose maxNodes, whose template?); hold it until the overlap is fixed.
			if memberships[m.Name] > 1 {
				logger.V(1).Info("skip machine matching several pools", "machine", m.Name)
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

// nodeForMachine builds the synthetic Node a Machine will present once Ready.
// It starts from the real Node object when one exists — a powered-off node keeps
// its Node object — because the labels the kubelet and node-feature discovery
// put there (kubernetes.io/os, arch, GPU product labels) are back the moment it
// boots, and an operator's cordon or taint survives the boot too. Over that come
// the pool Template labels, then the Machine's own Labels (Machine wins on
// conflict), and the Template taints. Lifecycle taints (node.kubernetes.io/*)
// only describe the node being down and are dropped, as is a cordon ONP placed
// itself, which the wake lifts. Allocatable is the room left once it is up (see
// syntheticAllocatable); bound holds the requests of pods still bound to it.
func nodeForMachine(m *v1alpha1.Machine, pool *v1alpha1.NodePool, real *corev1.Node, bound corev1.ResourceList) *corev1.Node {
	labels := map[string]string{corev1.LabelHostname: m.Spec.NodeName}
	var taints []corev1.Taint
	var cordoned bool
	if real != nil {
		for k, v := range real.Labels {
			labels[k] = v
		}
		taints = persistentTaints(real.Spec.Taints)
		_, onpCordon := real.Annotations[v1alpha1.AnnotationCordonedByONP]
		cordoned = real.Spec.Unschedulable && !onpCordon
	}
	for k, v := range pool.Spec.Template.Labels {
		labels[k] = v
	}
	for k, v := range m.Spec.Labels {
		labels[k] = v
	}
	for _, t := range pool.Spec.Template.Taints {
		taints, _ = mergeTaint(taints, t)
	}

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   m.Spec.NodeName,
			Labels: labels,
		},
		Spec: corev1.NodeSpec{Taints: taints, Unschedulable: cordoned},
		Status: corev1.NodeStatus{
			Allocatable: syntheticAllocatable(m.Spec.Capacity, real, bound),
		},
	}
}

// syntheticAllocatable is the room a Machine's node will offer once Ready. Per
// native resource (cpu, memory, ...) it takes the declared capacity capped by the
// allocatable the Node last reported — kubelet reservations come off the top. A
// declared extended resource (nvidia.com/gpu) is not capped: a device plugin that
// had not registered yet, or had just stopped, reports it as 0, and that 0 would
// keep the node from ever being woken for the pods it exists for. A resource the
// Machine does not declare comes from the Node.
// The requests of pods still bound to the node (DaemonSet pods stay bound while
// it is off, and the scheduler counts them) are subtracted, floored at zero.
func syntheticAllocatable(capacity corev1.ResourceList, real *corev1.Node, bound corev1.ResourceList) corev1.ResourceList {
	out := capacity.DeepCopy()
	if out == nil {
		out = corev1.ResourceList{}
	}
	if real != nil {
		for name, reported := range real.Status.Allocatable {
			declared, ok := out[name]
			if !ok || (!isExtendedResource(name) && reported.Cmp(declared) < 0) {
				out[name] = reported.DeepCopy()
			}
		}
	}
	for name, used := range bound {
		avail, ok := out[name]
		if !ok {
			continue
		}
		avail.Sub(used)
		if avail.Sign() < 0 {
			avail = resource.Quantity{}
		}
		out[name] = avail
	}
	return out
}

// isExtendedResource reports whether name is a domain-qualified resource such as
// a device plugin's nvidia.com/gpu, as opposed to a native one (cpu, memory,
// ephemeral-storage, hugepages-*).
func isExtendedResource(name corev1.ResourceName) bool {
	return strings.Contains(string(name), "/")
}

// boundRequests sums the requests of pods bound to nodeName that will still hold
// their place once it boots: everything but finished pods and pods already being
// deleted, which the kubelet clears as it comes up.
func boundRequests(ctx context.Context, c client.Client, nodeName string) (corev1.ResourceList, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.MatchingFields{IndexPodNodeName: nodeName}); err != nil {
		return nil, fmt.Errorf("list pods on node %q: %w", nodeName, err)
	}
	total := corev1.ResourceList{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for name, q := range resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{}) {
			sum := total[name]
			sum.Add(q)
			total[name] = sum
		}
	}
	return total, nil
}

// backingNode returns the named Node, or nil when it does not exist (a Machine
// whose node never joined).
func backingNode(ctx context.Context, c client.Client, nodeName string) (*corev1.Node, error) {
	if nodeName == "" {
		return nil, nil
	}
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get node %q: %w", nodeName, err)
	}
	return &node, nil
}

// persistentTaints returns the taints that will still be on the node once it
// boots: everything but the node.kubernetes.io/* lifecycle taints the node
// controller sets while a node is NotReady, unreachable or cordoned.
func persistentTaints(taints []corev1.Taint) []corev1.Taint {
	var kept []corev1.Taint
	for _, t := range taints {
		if strings.HasPrefix(t.Key, "node.kubernetes.io/") {
			continue
		}
		kept = append(kept, t)
	}
	return kept
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

// podVolumeAffinities returns the required node affinity of every
// PersistentVolume bound to a claim the pod mounts (persistentVolumeClaim and
// generic ephemeral volumes). An unbound or missing claim adds nothing: a
// WaitForFirstConsumer volume is provisioned where the pod lands, and a pod
// waiting on a claim that does not exist yet is not waiting on a node.
func podVolumeAffinities(ctx context.Context, reader client.Reader, pod *corev1.Pod) ([]*corev1.NodeSelector, error) {
	var required []*corev1.NodeSelector
	for _, vol := range pod.Spec.Volumes {
		var claim string
		switch {
		case vol.PersistentVolumeClaim != nil:
			claim = vol.PersistentVolumeClaim.ClaimName
		case vol.Ephemeral != nil:
			claim = pod.Name + "-" + vol.Name
		default:
			continue
		}
		var pvc corev1.PersistentVolumeClaim
		if err := reader.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: claim}, &pvc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get claim %s/%s: %w", pod.Namespace, claim, err)
		}
		if pvc.Spec.VolumeName == "" {
			continue
		}
		var pv corev1.PersistentVolume
		if err := reader.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get volume %s: %w", pvc.Spec.VolumeName, err)
		}
		if pv.Spec.NodeAffinity != nil && pv.Spec.NodeAffinity.Required != nil {
			required = append(required, pv.Spec.NodeAffinity.Required)
		}
	}
	return required, nil
}

// poolMemberships counts, per Machine name, how many of pools select it.
func poolMemberships(ctx context.Context, c client.Client, pools []v1alpha1.NodePool) (map[string]int, error) {
	counts := map[string]int{}
	for i := range pools {
		selector, err := metav1.LabelSelectorAsSelector(&pools[i].Spec.MachineSelector)
		if err != nil {
			continue
		}
		var machines v1alpha1.MachineList
		if err := c.List(ctx, &machines, client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return nil, fmt.Errorf("list machines for pool %q: %w", pools[i].Name, err)
		}
		for j := range machines.Items {
			counts[machines.Items[j].Name]++
		}
	}
	return counts, nil
}

// poolAtCap reports whether waking another member of this pool would exceed its
// maxNodes. A nil MaxNodes is unbounded. The count is the members that take a
// slot (see countsAgainstCap).
func (r *ScaleUpReconciler) poolAtCap(ctx context.Context, pool *v1alpha1.NodePool, members []v1alpha1.Machine) (bool, error) {
	if pool.Spec.MaxNodes == nil {
		return false, nil
	}
	var active int32
	for i := range members {
		counts, err := r.countsAgainstCap(ctx, &members[i])
		if err != nil {
			return false, err
		}
		if counts {
			active++
		}
	}
	return active >= *pool.Spec.MaxNodes, nil
}

// countsAgainstCap reports whether a member takes a maxNodes slot: it is active
// (isActive), or it is Failed while its Node is still up — a drain that timed out
// leaves the node uncordoned and serving, a power-off that never landed leaves it
// running — so it is a powered-on node the cap must see.
func (r *ScaleUpReconciler) countsAgainstCap(ctx context.Context, m *v1alpha1.Machine) (bool, error) {
	if isActive(m) {
		return true, nil
	}
	if m.Status.State != v1alpha1.MachineStateFailed {
		return false, nil
	}
	return nodeIsReady(ctx, r.Client, m.Spec.NodeName)
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

// settling reports whether a Machine reached Ready within readySettle — woken,
// most likely for this very pod, and not yet able to take it.
func (r *ScaleUpReconciler) settling(m *v1alpha1.Machine) bool {
	if m.Status.State != v1alpha1.MachineStateReady {
		return false
	}
	c := meta.FindStatusCondition(m.Status.Conditions, v1alpha1.ConditionReady)
	return c != nil && c.Status == metav1.ConditionTrue && r.Clock.Since(c.LastTransitionTime.Time) < readySettle
}

// isWaking reports whether a Machine already has a wake in progress that will
// satisfy a pending pod: it is Booting, or it is Off with the wake-now trigger
// already set (the MachineReconciler has not yet advanced it to Booting).
func isWaking(m *v1alpha1.Machine) bool {
	if m.Status.State == v1alpha1.MachineStateBooting {
		return true
	}
	if m.Status.State != v1alpha1.MachineStateOff || !wakeRequested(m) {
		return false
	}
	// A wake whose last power-on failed is not in flight: the controller keeps
	// retrying it, but the pod may be served by another Machine meanwhile.
	c := meta.FindStatusCondition(m.Status.Conditions, v1alpha1.ConditionPowerOnSucceeded)
	return c == nil || c.Status != metav1.ConditionFalse
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
