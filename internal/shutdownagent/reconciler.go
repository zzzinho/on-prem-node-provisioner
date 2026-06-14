// Package shutdownagent holds the reconciler run by onp-shutdown-agent, the
// privileged DaemonSet that lives on ONP-managed nodes. The agent is k8s-aware:
// it watches the Machine backing its own node and, when the controller advances
// that Machine to ShuttingDown, powers the host off with a graceful OS shutdown.
//
// Coordination is by CRD watch, not RPC (DESIGN.md 3.3): the controller writes
// Machine.status.state = ShuttingDown, the agent observes it here, and the
// controller later observes the Node going NotReady to finalize Off. Keeping the
// agent in its own package keeps its RBAC role (Machines read-only) separate
// from the onp-controller role.
package shutdownagent

import (
	"context"
	"fmt"
	"os/exec"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

// reasonPoweringOff is the Event reason emitted when the agent issues a host
// power-off. Kept as a constant so the string operators grep for stays stable.
const reasonPoweringOff = "PoweringOff"

// ShutdownReconciler powers its own node off when the backing Machine reaches
// ShuttingDown. It only ever acts on the Machine whose spec.nodeName equals
// NodeName; the watch is filtered to that Machine, and Reconcile re-checks it as
// defense-in-depth.
type ShutdownReconciler struct {
	client.Client

	// NodeName is the node this agent runs on, from the downward-API NODE_NAME.
	NodeName string

	// PowerOff issues the host power-off. It is injected so tests never shell
	// out; production wires SystemctlPowerOff.
	PowerOff func(context.Context) error

	// Recorder publishes Events on the Machine. Optional: nil disables Events.
	Recorder record.EventRecorder

	// poweredOff guards against issuing the power-off more than once. Once the
	// command is accepted the node is going down and this pod dies with it, so a
	// second issue would be at best redundant; a transient watch re-delivery
	// before the kernel actually halts must not re-run it.
	poweredOff sync.Once
}

// +kubebuilder:rbac:groups=onp.io,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile powers the node off when its Machine is ShuttingDown. It is a pure
// function of the Machine's state, so a re-delivery after a restart lands in the
// same place — and the sync.Once keeps a re-delivery from re-issuing the halt.
func (r *ShutdownReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var m v1alpha1.Machine
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		// Deleted between enqueue and now: nothing to power off.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Defense-in-depth: the watch predicate already filters to this node, but a
	// stale cache entry or a misconfigured field index must never let this agent
	// halt the wrong host.
	if m.Spec.NodeName != r.NodeName {
		return ctrl.Result{}, nil
	}

	if m.Status.State != v1alpha1.MachineStateShuttingDown {
		return ctrl.Result{}, nil
	}

	// Phase 1 powers nodes off through this agent. If a Machine selects a different
	// shutdown provider — a future hard-cut path that powers the node off out of
	// band — this agent must not also halt the host, or the node would be cut
	// twice. A nil or empty provider is the default (agent).
	if s := m.Spec.Shutdown; s != nil && s.Provider != "" && s.Provider != v1alpha1.ShutdownProviderAgent {
		logger.V(1).Info("machine selects a non-agent shutdown provider; not powering off here",
			"node", r.NodeName, "provider", s.Provider)
		return ctrl.Result{}, nil
	}

	var issued bool
	var powerErr error
	r.poweredOff.Do(func() {
		issued = true
		logger.Info("machine is ShuttingDown, powering node off", "node", r.NodeName)
		powerErr = r.PowerOff(ctx)
	})
	if !issued {
		// Already issued on an earlier reconcile; the node is on its way down.
		return ctrl.Result{}, nil
	}
	if powerErr != nil {
		// A failed power-off must surface so controller-runtime requeues; the
		// sync.Once has already fired, so the retry runs through the !issued
		// branch above and will NOT re-shell. We re-arm the Once so a genuine
		// transient failure can be retried.
		r.poweredOff = sync.Once{}
		return ctrl.Result{}, fmt.Errorf("power off node %q: %w", r.NodeName, powerErr)
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(&m, corev1.EventTypeNormal, reasonPoweringOff,
			"issued graceful power-off on node %q", r.NodeName)
	}
	// No requeue: the node is going away. The controller observes the Node going
	// NotReady and finalizes Machine.status.state = Off.
	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler to watch only the Machine backing this
// agent's node. The predicate runs on every Machine event the cache delivers and
// keeps the agent from reconciling (or even queuing) other nodes' Machines.
func (r *ShutdownReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ownNode := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		m, ok := obj.(*v1alpha1.Machine)
		return ok && m.Spec.NodeName == r.NodeName
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Machine{}, builder.WithPredicates(ownNode)).
		Named("shutdown").
		Complete(r)
}

// SystemctlPowerOff issues a graceful host shutdown from inside the agent's
// container. It enters PID 1's namespaces via nsenter and runs `systemctl
// poweroff`, which is a clean OS shutdown (not a sysrq hard halt) and re-arms
// NIC Wake-on-LAN on the boards ONP targets, so the controller can later wake
// the node again. The DaemonSet pod is privileged and shares the host PID
// namespace, which is what lets nsenter reach PID 1.
func SystemctlPowerOff(ctx context.Context) error {
	cmd := exec.CommandContext(ctx,
		"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid",
		"--", "systemctl", "poweroff")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nsenter systemctl poweroff: %w: %s", err, out)
	}
	return nil
}
