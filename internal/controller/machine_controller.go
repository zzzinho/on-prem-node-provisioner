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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
)

// MachineReconciler advances Machine.status.state along the wake path.
type MachineReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Registry *power.Registry
	// BootTimeout is how long a Machine may stay Booting before it is failed.
	BootTimeout time.Duration
	Recorder    record.EventRecorder
	// Clock reads the current time for boot-timeout math; injected so tests can
	// drive it with a fake clock. A PassiveClock is enough — the timeout is
	// evaluated each reconcile, not scheduled.
	Clock clock.PassiveClock
}

// +kubebuilder:rbac:groups=onp.io,resources=machines,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=onp.io,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
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
	default:
		// Failed, Draining, ShuttingDown: not part of the M2.2 wake path.
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
		m.Status.State = v1alpha1.MachineStateReady
		m.Status.BootStartTime = nil
		setCondition(m, v1alpha1.ConditionReady, metav1.ConditionTrue, reasonReady, "backing Node is Ready")
		if err := r.Status().Update(ctx, m); err != nil {
			return ctrl.Result{}, fmt.Errorf("move machine %q to Ready: %w", m.Name, err)
		}
		r.Recorder.Eventf(m, corev1.EventTypeNormal, reasonReady, "Node %q is Ready", m.Spec.NodeName)
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

// reconcileReady tidies a leftover wake-now annotation; Ready is otherwise
// stable in M2.2.
func (r *MachineReconciler) reconcileReady(ctx context.Context, m *v1alpha1.Machine) (ctrl.Result, error) {
	if _, ok := m.Annotations[v1alpha1.AnnotationWakeNow]; ok {
		if err := r.removeWakeAnnotation(ctx, m); err != nil {
			return ctrl.Result{}, err
		}
	}
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
