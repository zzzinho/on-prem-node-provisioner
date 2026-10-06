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
	"time"

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

// reasonPowerOffSkipped is the Event reason emitted when the agent declines a
// power-off because the host booted after it was requested.
const reasonPowerOffSkipped = "PowerOffSkipped"

// reasonPowerOffFailed is the Event reason emitted when the host power-off
// command fails.
const reasonPowerOffFailed = "PowerOffFailed"

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

	// BootTime reports when the host last booted; production wires HostBootTime.
	// nil skips the reboot check.
	BootTime func() (time.Time, error)

	// issued and issuedFor record the ShuttingDown episode — keyed by the
	// Machine's status.shutdownStartTime — this process already powered off for.
	// One episode, one power-off: a watch re-delivery of the same episode before
	// the kernel halts must not re-run it, and a later episode (the node came back
	// and was drained again) must not be skipped. MaxConcurrentReconciles is 1, so
	// the fields need no lock.
	issued    bool
	issuedFor time.Time
}

// +kubebuilder:rbac:groups=onp.io,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile powers the node off once per ShuttingDown episode of its Machine. A
// re-delivery of the same episode is a no-op; after an agent restart the episode
// record is gone, so the host's boot time is what keeps a node that already came
// back up from being powered off again.
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

	episode := shutdownEpisode(&m)
	if r.issued && r.issuedFor.Equal(episode) {
		return ctrl.Result{}, nil
	}
	if r.bootedAfter(ctx, episode) {
		// The host is up again after this power-off was requested: a board that
		// powered itself back on, or an operator who turned it on before the
		// controller saw it go down. Powering it off again would loop; leave it and
		// let the controller's shutdown timeout hand it to an operator.
		r.issued, r.issuedFor = true, episode
		if r.Recorder != nil {
			r.Recorder.Eventf(&m, corev1.EventTypeWarning, reasonPowerOffSkipped,
				"node %q booted after the power-off requested at %s; not powering it off again",
				r.NodeName, episode.Format(time.RFC3339))
		}
		return ctrl.Result{}, nil
	}

	logger.Info("machine is ShuttingDown, powering node off", "node", r.NodeName)
	if err := r.PowerOff(ctx); err != nil {
		// Not recorded as issued, so the requeue retries the power-off. Surface the
		// failure on the Machine: otherwise an operator only sees the controller's
		// ShutdownTimeout minutes later, without the command's output.
		if r.Recorder != nil {
			r.Recorder.Eventf(&m, corev1.EventTypeWarning, reasonPowerOffFailed,
				"power-off on node %q failed: %v", r.NodeName, err)
		}
		return ctrl.Result{}, fmt.Errorf("power off node %q: %w", r.NodeName, err)
	}
	r.issued, r.issuedFor = true, episode

	if r.Recorder != nil {
		r.Recorder.Eventf(&m, corev1.EventTypeNormal, reasonPoweringOff,
			"issued graceful power-off on node %q", r.NodeName)
	}
	return ctrl.Result{}, nil
}

// shutdownEpisode identifies a ShuttingDown episode by the instant the controller
// entered it; zero when the controller did not record one.
func shutdownEpisode(m *v1alpha1.Machine) time.Time {
	if m.Status.ShutdownStartTime == nil {
		return time.Time{}
	}
	return m.Status.ShutdownStartTime.Time
}

// bootedAfter reports whether the host booted after the power-off for episode was
// requested. An unknown episode or boot time answers false, so the agent falls
// back to powering off as asked rather than refusing a legitimate shutdown.
func (r *ShutdownReconciler) bootedAfter(ctx context.Context, episode time.Time) bool {
	if r.BootTime == nil || episode.IsZero() {
		return false
	}
	booted, err := r.BootTime()
	if err != nil {
		log.FromContext(ctx).Error(err, "read host boot time; skipping the reboot check")
		return false
	}
	return booted.After(episode)
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
