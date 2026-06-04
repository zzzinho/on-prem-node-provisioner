// Package controller holds the onp-controller reconcilers. MachineReconciler
// owns the Machine power lifecycle: it reacts to the onp.io/wake-now annotation,
// issues a power-on through the configured PowerProvider, and advances
// Machine.status.state — the single source of truth — as the backing Node comes
// up. Every reconcile recomputes state from status, the Node, and the
// annotation, so the controller is idempotent across restarts.
//
// M2.2 covers the wake path only (Off -> Booting -> Ready, with a boot-timeout
// to Failed). Drain and shutdown states are intentionally no-ops here.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
	"github.com/zzzinho/on-prem-node-provisioner/internal/power"
)

// IndexMachineNodeName is the field-index key over Machine.spec.nodeName. The
// Node->Machine mapper queries it so the Machine backing a Node is found by the
// link, not by assuming Machine.name == Node.name. main.go registers the same
// index on the manager's cache.
const IndexMachineNodeName = "spec.nodeName"

// IndexPodNodeName is the field-index key over Pod.spec.nodeName. reconcileDraining
// queries it to list a node's pods server-side rather than listing every pod in
// the cluster and filtering. main.go registers the matching index on the cache.
const IndexPodNodeName = "spec.nodeName"

// bootPollInterval bounds how often a Booting Machine is re-reconciled while we
// wait for its Node to go Ready, in case the Node watch misses the transition.
// It is also the requeue floor when the remaining boot budget is larger.
const bootPollInterval = 15 * time.Second

// Event reasons surfaced on Machine objects. Kept as constants so the strings
// operators grep for stay stable.
const (
	reasonWaking          = "Waking"
	reasonPowerOnFailed   = "PowerOnFailed"
	reasonReady           = "Ready"
	reasonBootTimeout     = "BootTimeout"
	reasonUnknownProvider = "UnknownProvider"
	reasonCannotPowerOn   = "CannotPowerOn"
	reasonPoweredOff      = "PoweredOff"
	reasonDraining        = "Draining"
	reasonDrainSucceeded  = "DrainSucceeded"
	reasonDrainTimeout    = "DrainTimeout"
	reasonUncordoned      = "Uncordoned"
	reasonShutdownTimeout = "ShutdownTimeout"
	reasonNodeLost        = "NodeLost"
)

// shutdownPollInterval bounds how often a ShuttingDown Machine is re-reconciled
// while we wait for its Node to go NotReady, in case the Node watch misses the
// transition. It mirrors bootPollInterval on the wake path.
const shutdownPollInterval = 15 * time.Second

// drainPollInterval bounds how often a Draining Machine is re-reconciled while
// evictions are in flight: after issuing evictions we requeue this often to
// re-list the node and check whether it has emptied. It is also how often we
// re-evaluate the drain timeout.
const drainPollInterval = 10 * time.Second

// defaultDrainTimeout is the drain budget used when the Machine's NodePool sets
// no drain.timeoutSeconds (or no pool matches). It mirrors DESIGN.md 3.3.
const defaultDrainTimeout = 300 * time.Second

// mirrorPodAnnotation marks a static (mirror) pod created by a kubelet from a
// manifest, not by the API server. Such pods cannot be evicted — deleting the
// mirror does nothing to the static pod — so the drain skips them.
const mirrorPodAnnotation = "kubernetes.io/config.mirror"

// MachineReconciler advances Machine.status.state along the wake path.
type MachineReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Registry *power.Registry
	// BootTimeout is how long a Machine may stay Booting before it is failed.
	BootTimeout time.Duration
	// ShutdownTimeout is how long a Machine may stay ShuttingDown — waiting for its
	// backing Node to go NotReady — before it is failed. It bounds the only state
	// that otherwise polls indefinitely: if the power-off never lands (the agent
	// did not run, or the board powered itself back on), the Machine fails instead
	// of polling forever.
	ShutdownTimeout time.Duration
	// NodeLossGracePeriod is how long a Ready Machine's backing Node may stay
	// NotReady — without ONP having started a drain — before the Machine falls back
	// to Off. The grace window absorbs brief kubelet blips so a transient NotReady
	// does not flap the Machine; a sustained loss (an external power-off) drops it
	// to Off, whence scale-up can wake it again.
	NodeLossGracePeriod time.Duration
	Recorder            record.EventRecorder
	// Clock reads the current time for boot-timeout math; injected so tests can
	// drive it with a fake clock. A PassiveClock is enough — the timeout is
	// evaluated each reconcile, not scheduled.
	Clock clock.PassiveClock
	// Evict issues a single pod eviction through the Eviction API. It is injected
	// so tests can stub it: the fake client's eviction subresource deletes the
	// pod unconditionally and never returns the PDB-blocked TooManyRequests we
	// must handle, so a real eviction path is untestable against it. nil falls
	// back to the controller-runtime Eviction subresource (wired in
	// SetupWithManager).
	Evict func(ctx context.Context, pod *corev1.Pod) error
}

// +kubebuilder:rbac:groups=onp.io,resources=machines,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=onp.io,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=onp.io,resources=nodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one Machine toward the correct state. It is a pure function
// of the Machine's status, the backing Node's readiness, and the wake-now
// annotation, so a fresh reconcile after a restart lands in the same place.
func (r *MachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var m v1alpha1.Machine
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		// Not found means the Machine was deleted between enqueue and now;
		// nothing to reconcile.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	switch m.Status.State {
	case "":
		return r.initialize(ctx, &m)
	case v1alpha1.MachineStateOff:
		return r.reconcileOff(ctx, &m)
	case v1alpha1.MachineStateBooting:
		return r.reconcileBooting(ctx, &m)
	case v1alpha1.MachineStateReady:
		return r.reconcileReady(ctx, &m)
	case v1alpha1.MachineStateDraining:
		return r.reconcileDraining(ctx, &m)
	case v1alpha1.MachineStateShuttingDown:
		return r.reconcileShuttingDown(ctx, &m)
	default:
		// Failed: a terminal state an operator must resolve by hand.
		logger.V(1).Info("no-op state", "state", m.Status.State)
		return ctrl.Result{}, nil
	}
}

// initialize stamps an unset Machine to Off so every later transition starts
// from a known state.
func (r *MachineReconciler) initialize(ctx context.Context, m *v1alpha1.Machine) (ctrl.Result, error) {
	m.Status.State = v1alpha1.MachineStateOff
	if err := r.Status().Update(ctx, m); err != nil {
		return ctrl.Result{}, fmt.Errorf("initialize machine %q to Off: %w", m.Name, err)
	}
	// Re-reconcile immediately so the wake-now annotation (if present) is acted
	// on without waiting for another event.
	return ctrl.Result{Requeue: true}, nil
}

// reconcileOff wakes the node when the wake-now annotation is set. A power-on
// failure leaves the Machine in Off and requeues; only an accepted command
// moves it to Booting, because PowerOn success is not boot success.
func (r *MachineReconciler) reconcileOff(ctx context.Context, m *v1alpha1.Machine) (ctrl.Result, error) {
	if !wakeRequested(m) {
		return ctrl.Result{}, nil
	}

	provider, ok := r.Registry.Get(m.Spec.Power.Provider)
	if !ok {
		r.Recorder.Eventf(m, corev1.EventTypeWarning, reasonUnknownProvider,
			"no power provider registered for %q", m.Spec.Power.Provider)
		// No state change: a provider may be registered on a future startup. We
		// do not requeue on a timer because registration is a static, startup
		// concern, not a transient one.
		return ctrl.Result{}, nil
	}
	if !provider.Capabilities().CanPowerOn {
		r.Recorder.Eventf(m, corev1.EventTypeWarning, reasonCannotPowerOn,
			"provider %q cannot power on", m.Spec.Power.Provider)
		return ctrl.Result{}, nil
	}

	if err := provider.PowerOn(ctx, m); err != nil {
		// Agent unreachable, bad config, etc. Stay Off, warn, and let
		// controller-runtime requeue with backoff so the next reconcile retries.
		r.Recorder.Eventf(m, corev1.EventTypeWarning, reasonPowerOnFailed,
			"power-on failed: %v", err)
		setCondition(m, v1alpha1.ConditionPowerOnSucceeded, metav1.ConditionFalse,
			reasonPowerOnFailed, err.Error())
		if uerr := r.Status().Update(ctx, m); uerr != nil {
			return ctrl.Result{}, fmt.Errorf("update machine %q after power-on failure: %w", m.Name, uerr)
		}
		return ctrl.Result{}, fmt.Errorf("power-on machine %q: %w", m.Name, err)
	}

	now := metav1.NewTime(r.Clock.Now())
	m.Status.State = v1alpha1.MachineStateBooting
	m.Status.BootStartTime = &now
	setCondition(m, v1alpha1.ConditionPowerOnSucceeded, metav1.ConditionTrue,
		reasonWaking, "power-on command accepted by provider")
	if err := r.Status().Update(ctx, m); err != nil {
		return ctrl.Result{}, fmt.Errorf("move machine %q to Booting: %w", m.Name, err)
	}
	r.Recorder.Eventf(m, corev1.EventTypeNormal, reasonWaking,
		"powered on via provider %q; waiting for Node %q to become Ready", m.Spec.Power.Provider, m.Spec.NodeName)

	return ctrl.Result{RequeueAfter: r.requeueForBoot(m)}, nil
}

// reconcileBooting promotes to Ready when the Node is Ready, fails on timeout,
// and otherwise requeues to keep polling for readiness.
func (r *MachineReconciler) reconcileBooting(ctx context.Context, m *v1alpha1.Machine) (ctrl.Result, error) {
	ready, err := r.nodeReady(ctx, m.Spec.NodeName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check node %q readiness: %w", m.Spec.NodeName, err)
	}
	if ready {
		// A node ONP cordoned during a prior scale-down is being woken back into
		// service; lift that cordon so it can host pods again. An operator's manual
		// cordon (no onp.io/cordoned-by-onp marker) is left alone.
		uncordoned, err := r.uncordonIfONPCordoned(ctx, m.Spec.NodeName)
		if err != nil {
			return ctrl.Result{}, err
		}
		m.Status.State = v1alpha1.MachineStateReady
		m.Status.BootStartTime = nil
		setCondition(m, v1alpha1.ConditionReady, metav1.ConditionTrue, reasonReady, "backing Node is Ready")
		if err := r.Status().Update(ctx, m); err != nil {
			return ctrl.Result{}, fmt.Errorf("move machine %q to Ready: %w", m.Name, err)
		}
		r.Recorder.Eventf(m, corev1.EventTypeNormal, reasonReady, "Node %q is Ready", m.Spec.NodeName)
		if uncordoned {
			r.Recorder.Eventf(m, corev1.EventTypeNormal, reasonUncordoned,
				"uncordoned Node %q on wake (it was cordoned by a prior scale-down)", m.Spec.NodeName)
		}
		// Clear the trigger so a stale annotation does not re-wake the node later.
		if err := r.removeWakeAnnotation(ctx, m); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if r.bootTimedOut(m) {
		m.Status.State = v1alpha1.MachineStateFailed
		setCondition(m, v1alpha1.ConditionReady, metav1.ConditionFalse, reasonBootTimeout,
			fmt.Sprintf("Node %q not Ready within %s", m.Spec.NodeName, r.BootTimeout))
		if err := r.Status().Update(ctx, m); err != nil {
			return ctrl.Result{}, fmt.Errorf("fail machine %q on boot timeout: %w", m.Name, err)
		}
		r.Recorder.Eventf(m, corev1.EventTypeWarning, reasonBootTimeout,
			"Node %q did not become Ready within %s", m.Spec.NodeName, r.BootTimeout)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: r.requeueForBoot(m)}, nil
}

// reconcileReady tidies a leftover wake-now annotation, starts a drain when the
// drain-now annotation is set, and otherwise watches for external node loss. A
// drain-now on a non-Ready Machine is ignored — only Ready -> Draining — so the
// trigger cannot interrupt a boot or a power-off already in flight.
func (r *MachineReconciler) reconcileReady(ctx context.Context, m *v1alpha1.Machine) (ctrl.Result, error) {
	// Tidy a leftover one-shot wake trigger first, so a stale "true" cannot re-wake
	// the node after it later powers off (e.g. once a drain below takes it down).
	if _, ok := m.Annotations[v1alpha1.AnnotationWakeNow]; ok {
		if err := r.removeWakeAnnotation(ctx, m); err != nil {
			return ctrl.Result{}, err
		}
	}

	// drain-now is ONP's own scale-down. A node going NotReady under a pending
	// drain is expected, so the external-loss check below is skipped when a drain
	// is requested.
	if drainRequested(m) {
		return r.startDraining(ctx, m)
	}

	// No drain pending: a Ready Machine whose backing Node has gone NotReady was
	// lost outside ONP (powered off by hand, crashed, partitioned). Fall back to
	// Off after a grace window so scale-up can wake it again.
	ready, err := r.nodeReady(ctx, m.Spec.NodeName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check node %q readiness: %w", m.Spec.NodeName, err)
	}
	if !ready {
		return r.reconcileReadyNodeLost(ctx, m)
	}
	if m.Status.NotReadySince != nil {
		// The Node recovered within the grace window; drop the stale loss anchor.
		if err := r.clearNotReadySince(ctx, m); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// startDraining moves a Ready Machine into Draining in response to drain-now and
// clears the one-shot trigger. The cordon -> evict -> power-off path runs from
// reconcileDraining.
func (r *MachineReconciler) startDraining(ctx context.Context, m *v1alpha1.Machine) (ctrl.Result, error) {
	now := metav1.NewTime(r.Clock.Now())
	m.Status.State = v1alpha1.MachineStateDraining
	m.Status.DrainStartTime = &now
	if err := r.Status().Update(ctx, m); err != nil {
		return ctrl.Result{}, fmt.Errorf("move machine %q to Draining: %w", m.Name, err)
	}
	r.Recorder.Eventf(m, corev1.EventTypeNormal, reasonDraining,
		"draining Node %q before power-off", m.Spec.NodeName)
	// Clear the one-shot trigger so a stale "true" does not re-drain a node the
	// operator later wakes, mirroring wake-now.
	if err := r.removeDrainAnnotation(ctx, m); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileReadyNodeLost handles a Ready Machine whose backing Node has gone
// NotReady outside any ONP-initiated drain. It anchors NotReadySince on first
// observation and requeues; once the Node has stayed NotReady for the whole grace
// window it drops the Machine to Off, whence scale-up can wake it again. The
// window absorbs brief kubelet blips so a transient NotReady does not flap state.
func (r *MachineReconciler) reconcileReadyNodeLost(ctx context.Context, m *v1alpha1.Machine) (ctrl.Result, error) {
	if m.Status.NotReadySince == nil {
		if err := r.stampNotReadySince(ctx, m); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.NodeLossGracePeriod}, nil
	}
	if elapsed := r.Clock.Since(m.Status.NotReadySince.Time); elapsed < r.NodeLossGracePeriod {
		wait := r.NodeLossGracePeriod - elapsed
		if wait < time.Second {
			wait = time.Second
		}
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	m.Status.State = v1alpha1.MachineStateOff
	m.Status.NotReadySince = nil
	setCondition(m, v1alpha1.ConditionReady, metav1.ConditionFalse, reasonNodeLost,
		fmt.Sprintf("Node %q stayed NotReady for %s without an ONP drain; assuming powered off", m.Spec.NodeName, r.NodeLossGracePeriod))
	if err := r.Status().Update(ctx, m); err != nil {
		return ctrl.Result{}, fmt.Errorf("move machine %q to Off after node loss: %w", m.Name, err)
	}
	r.Recorder.Eventf(m, corev1.EventTypeWarning, reasonNodeLost,
		"Node %q lost (NotReady for %s) with no ONP drain; Machine is Off", m.Spec.NodeName, r.NodeLossGracePeriod)
	return ctrl.Result{}, nil
}

// reconcileDraining cordons the backing Node and evicts its workload, then moves
// the Machine to ShuttingDown once the node is empty. On drain timeout it stops
// — uncordon + Failed + Event — rather than force-evicting, per the
// "조용히 데이터를 잃는 기본값은 만들지 않는다" default (DESIGN.md 3.3). The node stays
// cordoned across Draining -> ShuttingDown -> Off; only the timeout path
// uncordons.
func (r *MachineReconciler) reconcileDraining(ctx context.Context, m *v1alpha1.Machine) (ctrl.Result, error) {
	timeout, err := r.drainTimeout(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Timeout first: a node that will not drain must stop before we touch any
	// more pods, and the stop must uncordon so the operator can recover the node.
	if r.drainTimedOut(m, timeout) {
		if err := r.setCordon(ctx, m.Spec.NodeName, false); err != nil {
			return ctrl.Result{}, err
		}
		m.Status.State = v1alpha1.MachineStateFailed
		setCondition(m, v1alpha1.ConditionDrainSucceeded, metav1.ConditionFalse, reasonDrainTimeout,
			fmt.Sprintf("Node %q did not drain within %s", m.Spec.NodeName, timeout))
		if err := r.Status().Update(ctx, m); err != nil {
			return ctrl.Result{}, fmt.Errorf("fail machine %q on drain timeout: %w", m.Name, err)
		}
		r.Recorder.Eventf(m, corev1.EventTypeWarning, reasonDrainTimeout,
			"Node %q did not drain within %s; uncordoned and marked Failed", m.Spec.NodeName, timeout)
		return ctrl.Result{}, nil
	}

	if err := r.setCordon(ctx, m.Spec.NodeName, true); err != nil {
		return ctrl.Result{}, err
	}

	pods, err := r.evictablePods(ctx, m.Spec.NodeName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list evictable pods on node %q: %w", m.Spec.NodeName, err)
	}

	if len(pods) == 0 {
		// Node is empty: hand off to the power-off leg. Leave it cordoned — it is
		// on its way down, and reconcileShuttingDown + the agent finish it. Anchor
		// the shutdown timeout from here so a power-off that never lands fails
		// rather than polling forever.
		now := metav1.NewTime(r.Clock.Now())
		m.Status.State = v1alpha1.MachineStateShuttingDown
		m.Status.ShutdownStartTime = &now
		setCondition(m, v1alpha1.ConditionDrainSucceeded, metav1.ConditionTrue, reasonDrainSucceeded,
			fmt.Sprintf("Node %q drained; powering off", m.Spec.NodeName))
		if err := r.Status().Update(ctx, m); err != nil {
			return ctrl.Result{}, fmt.Errorf("move machine %q to ShuttingDown: %w", m.Name, err)
		}
		r.Recorder.Eventf(m, corev1.EventTypeNormal, reasonDrainSucceeded,
			"Node %q drained; moving to ShuttingDown", m.Spec.NodeName)
		return ctrl.Result{}, nil
	}

	for i := range pods {
		if err := r.evictPod(ctx, &pods[i]); err != nil {
			if apierrors.IsTooManyRequests(err) {
				// PDB blocked this eviction. Expected, not an error: keep draining
				// and re-check after the poll interval; the timeout above is the
				// backstop if the disruption budget never opens.
				log.FromContext(ctx).V(1).Info("eviction blocked by disruption budget",
					"pod", pods[i].Name, "node", m.Spec.NodeName)
				continue
			}
			return ctrl.Result{}, fmt.Errorf("evict pod %q on node %q: %w", pods[i].Name, m.Spec.NodeName, err)
		}
	}

	return ctrl.Result{RequeueAfter: drainPollInterval}, nil
}

// reconcileShuttingDown finalizes the power-off leg. The shutdown-agent issues
// the host poweroff in response to this same ShuttingDown state (coordination is
// by CRD watch, not RPC — DESIGN.md 3.3); the controller's job here is to observe
// the result. When the backing Node is no longer Ready (NotReady or the Node
// object gone) the poweroff has taken effect, so we land the Machine in Off.
// Until then we keep polling, because the Node going NotReady is the only signal
// that the node actually went down.
func (r *MachineReconciler) reconcileShuttingDown(ctx context.Context, m *v1alpha1.Machine) (ctrl.Result, error) {
	ready, err := r.nodeReady(ctx, m.Spec.NodeName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check node %q readiness: %w", m.Spec.NodeName, err)
	}
	if ready {
		// Poweroff not observed yet. If the node has not gone down within the
		// shutdown budget the power-off did not land (the agent never ran, or the
		// board powered itself back on) — fail rather than poll forever. The node is
		// left cordoned for the operator to inspect.
		if r.shutdownTimedOut(m) {
			m.Status.State = v1alpha1.MachineStateFailed
			m.Status.ShutdownStartTime = nil
			setCondition(m, v1alpha1.ConditionReady, metav1.ConditionFalse, reasonShutdownTimeout,
				fmt.Sprintf("Node %q still Ready %s after power-off was issued", m.Spec.NodeName, r.ShutdownTimeout))
			if err := r.Status().Update(ctx, m); err != nil {
				return ctrl.Result{}, fmt.Errorf("fail machine %q on shutdown timeout: %w", m.Name, err)
			}
			r.Recorder.Eventf(m, corev1.EventTypeWarning, reasonShutdownTimeout,
				"Node %q did not power off within %s; marked Failed", m.Spec.NodeName, r.ShutdownTimeout)
			return ctrl.Result{}, nil
		}
		// Requeue to keep polling; the Node watch will also enqueue us on the
		// NotReady transition, this is the safety net.
		return ctrl.Result{RequeueAfter: r.requeueForShutdown(m)}, nil
	}

	m.Status.State = v1alpha1.MachineStateOff
	m.Status.ShutdownStartTime = nil
	setCondition(m, v1alpha1.ConditionReady, metav1.ConditionFalse, reasonPoweredOff,
		fmt.Sprintf("Node %q is no longer Ready; power-off complete", m.Spec.NodeName))
	if err := r.Status().Update(ctx, m); err != nil {
		return ctrl.Result{}, fmt.Errorf("move machine %q to Off: %w", m.Name, err)
	}
	r.Recorder.Eventf(m, corev1.EventTypeNormal, reasonPoweredOff,
		"Node %q is no longer Ready; Machine is Off", m.Spec.NodeName)
	return ctrl.Result{}, nil
}

// nodeReady reports whether the named Node exists and has a Ready condition of
// True. A missing Node is not an error: the node may not have registered yet.
func (r *MachineReconciler) nodeReady(ctx context.Context, nodeName string) (bool, error) {
	if nodeName == "" {
		return false, nil
	}
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue, nil
		}
	}
	return false, nil
}

// bootTimedOut reports whether the Machine has been Booting longer than
// BootTimeout. A missing BootStartTime is treated as not-yet-timed-out so a
// half-written status never trips a false failure.
func (r *MachineReconciler) bootTimedOut(m *v1alpha1.Machine) bool {
	if m.Status.BootStartTime == nil {
		return false
	}
	return r.Clock.Since(m.Status.BootStartTime.Time) >= r.BootTimeout
}

// drainTimedOut reports whether the Machine has been Draining longer than the
// resolved timeout. A missing DrainStartTime is treated as not-yet-timed-out, by
// the same half-written-status reasoning as bootTimedOut.
func (r *MachineReconciler) drainTimedOut(m *v1alpha1.Machine, timeout time.Duration) bool {
	if m.Status.DrainStartTime == nil {
		return false
	}
	return r.Clock.Since(m.Status.DrainStartTime.Time) > timeout
}

// shutdownTimedOut reports whether the Machine has been ShuttingDown longer than
// ShutdownTimeout. A missing ShutdownStartTime is treated as not-yet-timed-out,
// by the same half-written-status reasoning as bootTimedOut/drainTimedOut.
func (r *MachineReconciler) shutdownTimedOut(m *v1alpha1.Machine) bool {
	if m.Status.ShutdownStartTime == nil {
		return false
	}
	return r.Clock.Since(m.Status.ShutdownStartTime.Time) > r.ShutdownTimeout
}

// drainTimeout resolves the drain budget for a Machine from its NodePool's
// drain.timeoutSeconds, falling back to defaultDrainTimeout when the pool sets
// no timeout or no pool matches. The first matching pool wins; pool overlap is a
// conflict surfaced elsewhere, not resolved here.
func (r *MachineReconciler) drainTimeout(ctx context.Context, m *v1alpha1.Machine) (time.Duration, error) {
	pool, err := poolForMachine(ctx, r.Client, m)
	if err != nil {
		return 0, err
	}
	if pool == nil || pool.Spec.Drain.TimeoutSeconds == nil {
		return defaultDrainTimeout, nil
	}
	return time.Duration(*pool.Spec.Drain.TimeoutSeconds) * time.Second, nil
}

// poolForMachine returns the first NodePool whose machineSelector matches the
// Machine's labels, or nil when none match. It mirrors NodePoolReconciler's
// selector logic (metav1.LabelSelectorAsSelector + a labels.Set match); the pool
// count is assumed small, so a full list per reconcile is acceptable. The drain
// (timeout resolution) and scale-down (disruption policy) paths share it.
func poolForMachine(ctx context.Context, c client.Client, m *v1alpha1.Machine) (*v1alpha1.NodePool, error) {
	var pools v1alpha1.NodePoolList
	if err := c.List(ctx, &pools); err != nil {
		return nil, fmt.Errorf("list nodepools for machine %q: %w", m.Name, err)
	}
	machineLabels := labels.Set(m.Labels)
	for i := range pools.Items {
		selector, err := metav1.LabelSelectorAsSelector(&pools.Items[i].Spec.MachineSelector)
		if err != nil {
			// A bad selector is the pool's own reconcile to report; skip it here.
			continue
		}
		if selector.Matches(machineLabels) {
			return &pools.Items[i], nil
		}
	}
	return nil, nil
}

// setCordon patches the Node's spec.unschedulable to the given value and, in the
// same patch, sets or clears the onp.io/cordoned-by-onp marker so a later wake
// can tell ONP's cordon from an operator's. It is a no-op when the node already
// matches both. A missing Node is not an error: the cordon target may have gone
// away, in which case there is nothing to drain.
func (r *MachineReconciler) setCordon(ctx context.Context, nodeName string, unschedulable bool) error {
	if nodeName == "" {
		return nil
	}
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get node %q to cordon: %w", nodeName, err)
	}
	_, marked := node.Annotations[v1alpha1.AnnotationCordonedByONP]
	// The marker tracks the cordon: present iff ONP holds the node cordoned.
	if node.Spec.Unschedulable == unschedulable && marked == unschedulable {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = unschedulable
	if unschedulable {
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}
		node.Annotations[v1alpha1.AnnotationCordonedByONP] = "true"
	} else {
		delete(node.Annotations, v1alpha1.AnnotationCordonedByONP)
	}
	if err := r.Patch(ctx, &node, patch); err != nil {
		return fmt.Errorf("set node %q unschedulable=%t: %w", nodeName, unschedulable, err)
	}
	return nil
}

// uncordonIfONPCordoned lifts a cordon ONP placed during a prior scale-down, so a
// node brought back into service is schedulable again. It uncordons only nodes
// carrying the onp.io/cordoned-by-onp marker, leaving an operator's manual cordon
// untouched. It returns whether it uncordoned, so the caller can emit an Event. A
// missing Node is not an error.
func (r *MachineReconciler) uncordonIfONPCordoned(ctx context.Context, nodeName string) (bool, error) {
	if nodeName == "" {
		return false, nil
	}
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get node %q to uncordon: %w", nodeName, err)
	}
	if _, marked := node.Annotations[v1alpha1.AnnotationCordonedByONP]; !marked {
		return false, nil
	}
	if err := r.setCordon(ctx, nodeName, false); err != nil {
		return false, err
	}
	return true, nil
}

// evictablePods lists the pods scheduled on the node and returns the ones a
// drain must evict. See evictablePodsOnNode for the exclusion rules.
func (r *MachineReconciler) evictablePods(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	return evictablePodsOnNode(ctx, r.Client, nodeName)
}

// evictablePodsOnNode lists the pods scheduled on the node and returns the ones a
// drain must evict — equivalently, the pods that make the node non-empty. It
// excludes pods that eviction cannot or should not move: DaemonSet-owned pods
// (rescheduled to the node regardless), mirror/static pods (not API-managed),
// already-terminating pods, and finished pods. The drain loop (what to evict) and
// the scale-down path (is the node empty) both read it, so a node is detected
// "empty" by exactly the condition the drain later confirms. nodeName "" yields
// no pods.
func evictablePodsOnNode(ctx context.Context, c client.Client, nodeName string) ([]corev1.Pod, error) {
	if nodeName == "" {
		return nil, nil
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.MatchingFields{IndexPodNodeName: nodeName}); err != nil {
		return nil, err
	}
	evictable := make([]corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		if isEvictable(&pods.Items[i]) {
			evictable = append(evictable, pods.Items[i])
		}
	}
	return evictable, nil
}

// evictPod issues one eviction through the injected Evict func, or the
// controller-runtime Eviction subresource when none is wired.
func (r *MachineReconciler) evictPod(ctx context.Context, pod *corev1.Pod) error {
	if r.Evict != nil {
		return r.Evict(ctx, pod)
	}
	return evictViaSubResource(ctx, r.Client, pod)
}

// evictViaSubResource POSTs a policy/v1 Eviction for the pod through the
// client's eviction subresource — the Eviction API, so PodDisruptionBudgets are
// honored (a blocked eviction comes back as TooManyRequests). It never deletes
// the pod directly. This is the default Evict wired in production.
func evictViaSubResource(ctx context.Context, c client.Client, pod *corev1.Pod) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	return c.SubResource("eviction").Create(ctx, pod, eviction)
}

// isEvictable reports whether a drain should evict the pod. The exclusions match
// kubectl drain's defaults for an unforced drain.
func isEvictable(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}
	if _, ok := pod.Annotations[mirrorPodAnnotation]; ok {
		return false
	}
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return false
		}
	}
	// TODO(M5): honor the onp.io/do-not-disrupt annotation here as a hard guard.
	return true
}

// requeueForBoot returns how long to wait before re-reconciling a Booting
// Machine: the smaller of the poll interval and the remaining boot budget, so
// we wake up close to the deadline even when polling slower.
func (r *MachineReconciler) requeueForBoot(m *v1alpha1.Machine) time.Duration {
	if m.Status.BootStartTime == nil {
		return bootPollInterval
	}
	remaining := r.BootTimeout - r.Clock.Since(m.Status.BootStartTime.Time)
	if remaining < bootPollInterval {
		if remaining < 0 {
			remaining = 0
		}
		return remaining
	}
	return bootPollInterval
}

// requeueForShutdown returns how long to wait before re-reconciling a
// ShuttingDown Machine: the smaller of the poll interval and the remaining
// shutdown budget, so we wake up close to the deadline even when polling slower.
// It mirrors requeueForBoot.
func (r *MachineReconciler) requeueForShutdown(m *v1alpha1.Machine) time.Duration {
	if m.Status.ShutdownStartTime == nil {
		return shutdownPollInterval
	}
	remaining := r.ShutdownTimeout - r.Clock.Since(m.Status.ShutdownStartTime.Time)
	if remaining < shutdownPollInterval {
		if remaining < 0 {
			remaining = 0
		}
		return remaining
	}
	return shutdownPollInterval
}

// removeWakeAnnotation strips the one-shot wake trigger via a patch so the
// removal does not clobber concurrent spec edits.
func (r *MachineReconciler) removeWakeAnnotation(ctx context.Context, m *v1alpha1.Machine) error {
	if _, ok := m.Annotations[v1alpha1.AnnotationWakeNow]; !ok {
		return nil
	}
	patch := client.MergeFrom(m.DeepCopy())
	delete(m.Annotations, v1alpha1.AnnotationWakeNow)
	if err := r.Patch(ctx, m, patch); err != nil {
		return fmt.Errorf("remove wake-now annotation from machine %q: %w", m.Name, err)
	}
	return nil
}

// wakeRequested reports whether the wake-now annotation is set to its trigger
// value.
func wakeRequested(m *v1alpha1.Machine) bool {
	return m.Annotations[v1alpha1.AnnotationWakeNow] == v1alpha1.AnnotationWakeNowValue
}

// removeDrainAnnotation strips the one-shot drain trigger via a patch so the
// removal does not clobber concurrent spec edits, mirroring removeWakeAnnotation.
func (r *MachineReconciler) removeDrainAnnotation(ctx context.Context, m *v1alpha1.Machine) error {
	if _, ok := m.Annotations[v1alpha1.AnnotationDrainNow]; !ok {
		return nil
	}
	patch := client.MergeFrom(m.DeepCopy())
	delete(m.Annotations, v1alpha1.AnnotationDrainNow)
	if err := r.Patch(ctx, m, patch); err != nil {
		return fmt.Errorf("remove drain-now annotation from machine %q: %w", m.Name, err)
	}
	return nil
}

// drainRequested reports whether the drain-now annotation is set to its trigger
// value.
func drainRequested(m *v1alpha1.Machine) bool {
	return m.Annotations[v1alpha1.AnnotationDrainNow] == v1alpha1.AnnotationDrainNowValue
}

// stampNotReadySince records the instant a Ready Machine's Node was first seen
// NotReady, the anchor for the node-loss grace window. It uses a MergeFrom status
// patch so it sends only that one field and does not clobber concurrent status
// writes (the scale-down path patches emptySince on the same object).
func (r *MachineReconciler) stampNotReadySince(ctx context.Context, m *v1alpha1.Machine) error {
	orig := m.DeepCopy()
	now := metav1.NewTime(r.Clock.Now())
	m.Status.NotReadySince = &now
	if err := r.Status().Patch(ctx, m, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("stamp notReadySince on machine %q: %w", m.Name, err)
	}
	return nil
}

// clearNotReadySince drops the node-loss anchor when the Node is Ready again. It
// is a no-op when the anchor is already unset. The patch nils the field via a JSON
// merge patch (omitempty -> the diff sends notReadySince: null), mirroring the
// scale-down path's clearEmptySince.
func (r *MachineReconciler) clearNotReadySince(ctx context.Context, m *v1alpha1.Machine) error {
	if m.Status.NotReadySince == nil {
		return nil
	}
	orig := m.DeepCopy()
	m.Status.NotReadySince = nil
	if err := r.Status().Patch(ctx, m, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("clear notReadySince on machine %q: %w", m.Name, err)
	}
	return nil
}

// setCondition writes a standard condition, stamping LastTransitionTime only on
// an actual status change.
func setCondition(m *v1alpha1.Machine, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: m.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// SetupWithManager wires the reconciler: it owns Machine objects and watches
// Nodes, fanning a Node event out to the Machine that links it via nodeName.
func (r *MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Machine{}).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.machinesForNode),
		).
		Named("machine").
		Complete(r)
}

// machinesForNode maps a Node event to reconcile requests for every Machine
// whose spec.nodeName points at it, found through the field index.
func (r *MachineReconciler) machinesForNode(ctx context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}
	var machines v1alpha1.MachineList
	if err := r.List(ctx, &machines, client.MatchingFields{IndexMachineNodeName: node.Name}); err != nil {
		log.FromContext(ctx).Error(err, "list machines for node", "node", node.Name)
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
