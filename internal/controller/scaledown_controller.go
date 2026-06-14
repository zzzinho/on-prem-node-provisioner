package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

// minConsolidateRequeue floors the requeue computed from the remaining
// consolidateAfter window, so a near-zero remainder still yields a real wait
// rather than a hot loop racing the clock's resolution. It mirrors
// minCooldownRequeue on the scale-up path.
const minConsolidateRequeue = time.Second

// scaleDownBlockedRequeue is how often a node that is empty-and-elapsed but held
// back by a pool guard (minNodes floor or maxConcurrent cap) is re-checked. The
// guards depend on other members' state, which does not enqueue this Machine, so
// a timed re-check is how the drain fires once the floor rises or a slot frees.
const scaleDownBlockedRequeue = 30 * time.Second

// reasonScaleDown is the Event reason emitted on a Machine when it has stayed
// empty long enough that ONP triggers its drain and power-off.
const reasonScaleDown = "ScaleDown"

// reasonScaleDownBlocked is the Event reason emitted on a Machine that is ready to
// scale down but held back by a pool guard — the minNodes floor or the
// maxConcurrent cap.
const reasonScaleDownBlocked = "ScaleDownBlocked"

// ScaleDownReconciler powers off idle nodes: when a Ready Machine in a WhenEmpty
// pool has carried no evictable workload for the pool's consolidateAfter, it sets
// the onp.io/drain-now annotation and lets MachineReconciler run the actual
// Ready -> Draining -> ShuttingDown -> Off path. Like ScaleUpReconciler it does
// selection only — never draining or powering off directly — so the manual
// (operator-set annotation) and automatic paths stay one code path.
//
// It is the mirror of the scale-up path: scale-up wakes a node for a pending pod;
// scale-down powers one off once it is empty. Membership-floor (minNodes),
// per-pool concurrency (maxConcurrent), scaleDown cooldown and the
// onp.io/do-not-disrupt guard are M5 — this milestone is empty-detection plus the
// consolidateAfter timer plus the drain trigger.
type ScaleDownReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Clock reads the current time for the consolidateAfter timer; injected so
	// tests can drive it with a fake clock. A PassiveClock is enough — the timer is
	// evaluated each reconcile against status.emptySince, not scheduled.
	Clock clock.PassiveClock
}

// +kubebuilder:rbac:groups=onp.io,resources=machines,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=onp.io,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=onp.io,resources=nodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=onp.io,resources=nodepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile evaluates one Machine for scale-down. It is a pure function of the
// Machine's state, its pool's disruption policy, the backing Node's emptiness and
// status.emptySince, so a fresh reconcile after a restart lands in the same place.
// status.emptySince is the only thing it writes (other than the one-shot
// drain-now trigger), and it is cleared the moment the Machine is no longer a
// scale-down candidate so a re-woken node never drains on a stale observation.
func (r *ScaleDownReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var m v1alpha1.Machine
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		// Not found means the Machine was deleted between enqueue and now.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only a Ready Machine is a scale-down candidate. A booting, draining,
	// shutting-down or off node has no empty timer to keep, so clear a stale
	// anchor and stop — this is also how the timer is reset once the trigger below
	// moves the Machine out of Ready.
	if m.Status.State != v1alpha1.MachineStateReady {
		return ctrl.Result{}, r.clearEmptySince(ctx, &m)
	}

	// A drain already requested (by a prior reconcile here, or by an operator's
	// manual annotation) means the node is on its way down; do not re-evaluate.
	if drainRequested(&m) {
		return ctrl.Result{}, nil
	}

	// A Node the operator marked do-not-disrupt is exempt from automatic scale-down
	// entirely: never start its empty timer, and drop any anchor from before the
	// annotation was added.
	protected, err := r.nodeDoNotDisrupt(ctx, m.Spec.NodeName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if protected {
		return ctrl.Result{}, r.clearEmptySince(ctx, &m)
	}

	// A Machine matching more than one pool has ambiguous disruption policy — whose
	// consolidateAfter, whose minNodes/maxConcurrent? Rather than act on a guessed
	// pool, hold automatic scale-down and warn until an operator resolves the
	// overlap (DESIGN.md 3.2). Clear any stale empty anchor so a resolved overlap
	// starts a fresh timer.
	pools, err := matchingPools(ctx, r.Client, &m)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(pools) > 1 {
		r.Recorder.Eventf(&m, corev1.EventTypeWarning, reasonPoolConflict,
			"Machine %q matches %d NodePools (%s); holding automatic scale-down until the overlap is resolved",
			m.Name, len(pools), poolNames(pools))
		return ctrl.Result{}, r.clearEmptySince(ctx, &m)
	}
	var pool *v1alpha1.NodePool
	if len(pools) == 1 {
		pool = &pools[0]
	}
	after, enabled := consolidateAfter(pool)
	if !enabled {
		// No pool matches, the policy is not WhenEmpty, or consolidateAfter is
		// unset: automatic scale-down is off for this Machine. Clear any stale
		// anchor so it does not fire if the policy is enabled again later.
		return ctrl.Result{}, r.clearEmptySince(ctx, &m)
	}

	empty, err := r.nodeEmpty(ctx, m.Spec.NodeName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check node %q emptiness: %w", m.Spec.NodeName, err)
	}
	if !empty {
		// The node carries workload; reset the timer so it must stay empty afresh.
		return ctrl.Result{}, r.clearEmptySince(ctx, &m)
	}

	// The node is empty. Anchor the timer on first observation and wait out the
	// window; the requeue re-checks both emptiness and elapsed time.
	if m.Status.EmptySince == nil {
		if err := r.stampEmptySince(ctx, &m); err != nil {
			return ctrl.Result{}, err
		}
		logger.V(1).Info("node observed empty; starting consolidateAfter timer",
			"machine", m.Name, "node", m.Spec.NodeName, "after", after)
		return ctrl.Result{RequeueAfter: after}, nil
	}

	if elapsed := r.Clock.Since(m.Status.EmptySince.Time); elapsed < after {
		wait := after - elapsed
		if wait < minConsolidateRequeue {
			wait = minConsolidateRequeue
		}
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	// Empty for the whole consolidateAfter. Before triggering, clear the three
	// pool-level scale-down guards; any one of them holds the drain back.
	return r.triggerScaleDown(ctx, &m, pool, after)
}

// triggerScaleDown applies the pool's scale-down guards and, if all pass, stamps
// the cooldown anchor and sets drain-now. The guards are evaluated against the
// pool's current membership at trigger time (not when the timer started), so a
// node that woke or a drain that finished in the meantime is accounted for.
func (r *ScaleDownReconciler) triggerScaleDown(ctx context.Context, m *v1alpha1.Machine, pool *v1alpha1.NodePool, after time.Duration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// cooldown.scaleDown: rate-limit power-off decisions across the pool, mirroring
	// cooldown.scaleUp. Requeue exactly when the interval lifts.
	if until := r.coolingDownUntil(pool); !until.IsZero() {
		wait := until.Sub(r.Clock.Now())
		if wait < minConsolidateRequeue {
			wait = minConsolidateRequeue
		}
		logger.V(1).Info("scale-down on cooldown; waiting", "machine", m.Name, "after", wait)
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	members, err := r.poolMembers(ctx, pool)
	if err != nil {
		return ctrl.Result{}, err
	}
	keptOn, scalingDown := poolScaleDownCounts(members)

	// minNodes floor: never drain a node that would drop the pool's kept-on count
	// below its floor. keptOn includes this Machine (Ready, not yet draining), so
	// draining it leaves keptOn-1; refuse when that would breach minNodes.
	if keptOn <= pool.Spec.MinNodes {
		r.Recorder.Eventf(m, corev1.EventTypeNormal, reasonScaleDownBlocked,
			"not draining node %q: pool %q at its minNodes floor (%d kept on, min %d)",
			m.Spec.NodeName, pool.Name, keptOn, pool.Spec.MinNodes)
		return ctrl.Result{RequeueAfter: scaleDownBlockedRequeue}, nil
	}

	// maxConcurrent: pace concurrent drains so a wave of idle nodes is retired a
	// few at a time. A nil cap reads as 1 (the safe default for objects created
	// before the field existed).
	maxConcurrent := int32(1)
	if pool.Spec.Disruption.MaxConcurrent != nil {
		maxConcurrent = *pool.Spec.Disruption.MaxConcurrent
	}
	if scalingDown >= maxConcurrent {
		r.Recorder.Eventf(m, corev1.EventTypeNormal, reasonScaleDownBlocked,
			"deferring drain of node %q: pool %q already draining %d (maxConcurrent %d)",
			m.Spec.NodeName, pool.Name, scalingDown, maxConcurrent)
		return ctrl.Result{RequeueAfter: scaleDownBlockedRequeue}, nil
	}

	// All guards pass. Stamp the cooldown anchor before triggering, so a drain that
	// then fails to patch the Machine has not silently skipped rate-limiting
	// (mirrors ScaleUpReconciler.stampScaleUp ordering).
	if err := r.stampScaleDown(ctx, pool); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.requestDrain(ctx, m); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(m, corev1.EventTypeNormal, reasonScaleDown,
		"node %q empty for %s; draining and powering off", m.Spec.NodeName, after)
	return ctrl.Result{}, nil
}

// nodeEmpty reports whether the node carries no workload, by the same definition
// the drain uses for its empty check (DaemonSet, mirror, terminating and finished
// pods do not count). A do-not-disrupt pod is workload, so a node carrying one is
// never empty — which is how a Pod-level do-not-disrupt keeps its node out of
// automatic scale-down without any special case here.
func (r *ScaleDownReconciler) nodeEmpty(ctx context.Context, nodeName string) (bool, error) {
	pods, err := workloadPodsOnNode(ctx, r.Client, nodeName)
	if err != nil {
		return false, err
	}
	return len(pods) == 0, nil
}

// nodeDoNotDisrupt reports whether the backing Node carries the do-not-disrupt
// annotation, which exempts the whole node from automatic scale-down. A missing
// Node is not protected: there is nothing ONP could power off.
func (r *ScaleDownReconciler) nodeDoNotDisrupt(ctx context.Context, nodeName string) (bool, error) {
	if nodeName == "" {
		return false, nil
	}
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get node %q for do-not-disrupt check: %w", nodeName, err)
	}
	return node.Annotations[v1alpha1.AnnotationDoNotDisrupt] == v1alpha1.AnnotationDoNotDisruptValue, nil
}

// stampEmptySince records the instant the node was first observed empty, the
// anchor for the consolidateAfter timer. It uses a MergeFrom status patch so it
// sends only that one field and does not clobber concurrent status writes
// (MachineReconciler patches state/conditions on the same object).
func (r *ScaleDownReconciler) stampEmptySince(ctx context.Context, m *v1alpha1.Machine) error {
	orig := m.DeepCopy()
	now := metav1.NewTime(r.Clock.Now())
	m.Status.EmptySince = &now
	if err := r.Status().Patch(ctx, m, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("stamp emptySince on machine %q: %w", m.Name, err)
	}
	return nil
}

// clearEmptySince drops the empty-timer anchor when the node is no longer a
// scale-down candidate. It is a no-op when the anchor is already unset, so the
// common "not empty / not Ready" reconcile writes nothing. The patch nils the
// field via a JSON merge patch (omitempty -> the diff sends emptySince: null).
func (r *ScaleDownReconciler) clearEmptySince(ctx context.Context, m *v1alpha1.Machine) error {
	if m.Status.EmptySince == nil {
		return nil
	}
	orig := m.DeepCopy()
	m.Status.EmptySince = nil
	if err := r.Status().Patch(ctx, m, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("clear emptySince on machine %q: %w", m.Name, err)
	}
	return nil
}

// requestDrain sets the one-shot drain-now annotation via a merge patch so it
// does not clobber concurrent spec edits, mirroring ScaleUpReconciler.requestWake.
func (r *ScaleDownReconciler) requestDrain(ctx context.Context, m *v1alpha1.Machine) error {
	patch := client.MergeFrom(m.DeepCopy())
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations[v1alpha1.AnnotationDrainNow] = v1alpha1.AnnotationDrainNowValue
	if err := r.Patch(ctx, m, patch); err != nil {
		return fmt.Errorf("set drain-now annotation on machine %q: %w", m.Name, err)
	}
	return nil
}

// consolidateAfter reads the pool's scale-down window and whether automatic
// scale-down is enabled. It is enabled only when the pool sets
// consolidationPolicy: WhenEmpty *and* a non-nil consolidateAfter — both are
// explicit opt-ins, so a node is never powered off without an operator-chosen
// delay. A nil pool (no pool matches the Machine) is disabled.
func consolidateAfter(pool *v1alpha1.NodePool) (time.Duration, bool) {
	if pool == nil {
		return 0, false
	}
	if pool.Spec.Disruption.ConsolidationPolicy != v1alpha1.ConsolidationPolicyWhenEmpty {
		return 0, false
	}
	ca := pool.Spec.Disruption.ConsolidateAfter
	if ca == nil {
		return 0, false
	}
	return ca.Duration, true
}

// poolMembers lists the Machines matching the pool's machineSelector. The scale-
// down guards count over this set. It mirrors the selector logic the scale-up and
// nodepool reconcilers use.
func (r *ScaleDownReconciler) poolMembers(ctx context.Context, pool *v1alpha1.NodePool) ([]v1alpha1.Machine, error) {
	selector, err := metav1.LabelSelectorAsSelector(&pool.Spec.MachineSelector)
	if err != nil {
		return nil, fmt.Errorf("convert machineSelector for nodepool %q: %w", pool.Name, err)
	}
	var machines v1alpha1.MachineList
	if err := r.List(ctx, &machines, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list machines for pool %q: %w", pool.Name, err)
	}
	return machines.Items, nil
}

// poolScaleDownCounts splits a pool's members into those kept powered on (active
// and not already being retired) and those already scaling down. keptOn anchors
// the minNodes floor; scalingDown anchors the maxConcurrent cap. A member already
// on its way down is counted in scalingDown only, never keptOn, so it neither
// props up the floor nor is double-counted.
func poolScaleDownCounts(members []v1alpha1.Machine) (keptOn, scalingDown int32) {
	for i := range members {
		m := &members[i]
		if isScalingDown(m) {
			scalingDown++
			continue
		}
		if isActive(m) {
			keptOn++
		}
	}
	return keptOn, scalingDown
}

// isScalingDown reports whether a Machine is already on its way down: draining,
// powering off, or triggered to drain by a pending drain-now. Such a member does
// not count toward the pool's kept-on floor and does count against maxConcurrent.
func isScalingDown(m *v1alpha1.Machine) bool {
	switch m.Status.State {
	case v1alpha1.MachineStateDraining, v1alpha1.MachineStateShuttingDown:
		return true
	default:
		return drainRequested(m)
	}
}

// coolingDownUntil returns the instant a pool's scale-down cooldown lifts, or the
// zero time if the pool is not rate-limited right now (no cooldown configured, no
// prior scale-down, or the interval has already elapsed). It mirrors the scale-up
// path's coolingUntil.
func (r *ScaleDownReconciler) coolingDownUntil(pool *v1alpha1.NodePool) time.Time {
	cd := pool.Spec.Cooldown.ScaleDown
	last := pool.Status.LastScaleDownTime
	if cd == nil || last == nil {
		return time.Time{}
	}
	expiry := last.Time.Add(cd.Duration)
	if !r.Clock.Now().Before(expiry) {
		return time.Time{}
	}
	return expiry
}

// stampScaleDown records that ONP just triggered a scale-down in this pool by
// writing status.LastScaleDownTime, the anchor for cooldown.scaleDown. It uses a
// MergeFrom status patch so it sends only that one field and does not clobber the
// membership counts NodePoolReconciler writes. It mirrors
// ScaleUpReconciler.stampScaleUp.
func (r *ScaleDownReconciler) stampScaleDown(ctx context.Context, pool *v1alpha1.NodePool) error {
	orig := pool.DeepCopy()
	now := metav1.NewTime(r.Clock.Now())
	pool.Status.LastScaleDownTime = &now
	if err := r.Status().Patch(ctx, pool, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("stamp scale-down time on nodepool %q: %w", pool.Name, err)
	}
	return nil
}

// SetupWithManager wires the reconciler: it reconciles Machines and watches Pods,
// fanning a Pod event (filtered to the ones that flip whether its node is empty)
// out to the Machine backing that pod's node. The Machine watch covers state
// transitions into and out of Ready; the Pod watch covers the node emptying.
func (r *ScaleDownReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Machine{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.machinesForPod),
			builder.WithPredicates(podEmptinessPredicate()),
		).
		Named("scaledown").
		Complete(r)
}

// machinesForPod maps a Pod event to a reconcile of the Machine backing the pod's
// node, found through the spec.nodeName field index. An unscheduled pod (no
// NodeName) maps to nothing.
func (r *ScaleDownReconciler) machinesForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}
	var machines v1alpha1.MachineList
	if err := r.List(ctx, &machines, client.MatchingFields{IndexMachineNodeName: pod.Spec.NodeName}); err != nil {
		log.FromContext(ctx).Error(err, "list machines for pod", "pod", pod.Namespace+"/"+pod.Name, "node", pod.Spec.NodeName)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(machines.Items))
	for i := range machines.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: machines.Items[i].Name},
		})
	}
	return requests
}

// podEmptinessPredicate admits only Pod events that change whether the pod makes
// its node non-empty, so the scale-down queue is not woken by routine status
// heartbeats. A created/deleted scheduled pod flips emptiness; an update matters
// only when the pod's evictability changes (it started terminating or reached a
// terminal phase).
func podEmptinessPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			pod, ok := e.Object.(*corev1.Pod)
			return ok && pod.Spec.NodeName != "" && isWorkload(pod)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			pod, ok := e.Object.(*corev1.Pod)
			return ok && pod.Spec.NodeName != ""
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, ok1 := e.ObjectOld.(*corev1.Pod)
			newPod, ok2 := e.ObjectNew.(*corev1.Pod)
			if !ok1 || !ok2 || newPod.Spec.NodeName == "" {
				return false
			}
			return isWorkload(oldPod) != isWorkload(newPod)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
