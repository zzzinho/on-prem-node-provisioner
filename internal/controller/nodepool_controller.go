package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

// NodePoolReconciler aggregates pool membership: it counts the Machines that
// match a NodePool's machineSelector and how many of them are Ready, then
// writes those counts to status. M3.0 is membership and aggregation only —
// scale-up, candidate selection, cooldown and maxNodes enforcement live in
// later milestones.
type NodePoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=onp.io,resources=nodepools,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=onp.io,resources=nodepools/status,verbs=get;update;patch

// Reconcile recomputes a NodePool's membership counts from the Machines that
// match its selector and writes them to status. It is a pure function of the
// matching Machines, so a fresh reconcile after a restart lands in the same
// place; it skips the status write when nothing changed to avoid update churn.
func (r *NodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool v1alpha1.NodePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		// Not found means the NodePool was deleted between enqueue and now;
		// nothing to reconcile.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	selector, err := metav1.LabelSelectorAsSelector(&pool.Spec.MachineSelector)
	if err != nil {
		// A malformed selector is an operator error in spec; it will not fix
		// itself on requeue, so we report it and stop rather than hot-loop.
		return ctrl.Result{}, fmt.Errorf("convert machineSelector for nodepool %q: %w", pool.Name, err)
	}

	total, ready, err := r.countMembers(ctx, selector)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("count members of nodepool %q: %w", pool.Name, err)
	}

	if pool.Status.TotalMachines == total && pool.Status.ReadyMachines == ready {
		// Membership is unchanged; skip the write so a Machine event that does
		// not move the counts does not churn the NodePool's resourceVersion.
		return ctrl.Result{}, nil
	}

	pool.Status.TotalMachines = total
	pool.Status.ReadyMachines = ready
	if err := r.Status().Update(ctx, &pool); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for nodepool %q: %w", pool.Name, err)
	}
	return ctrl.Result{}, nil
}

// countMembers returns how many Machines match the selector and how many of
// those are in the Ready state.
func (r *NodePoolReconciler) countMembers(ctx context.Context, selector labels.Selector) (total, ready int32, err error) {
	var machines v1alpha1.MachineList
	if err := r.List(ctx, &machines, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return 0, 0, err
	}
	for i := range machines.Items {
		total++
		if machines.Items[i].Status.State == v1alpha1.MachineStateReady {
			ready++
		}
	}
	return total, ready, nil
}

// SetupWithManager wires the reconciler: it owns NodePool objects and watches
// Machines, fanning a Machine event out to every NodePool whose selector
// matches that Machine's labels.
func (r *NodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.NodePool{}).
		Watches(
			&v1alpha1.Machine{},
			handler.EnqueueRequestsFromMapFunc(r.poolsForMachine),
		).
		Named("nodepool").
		Complete(r)
}

// poolsForMachine maps a Machine event to reconcile requests for every NodePool
// whose machineSelector matches the Machine's labels. There is no field index
// for "pools matching these labels", so it lists all NodePools and tests each
// selector; the pool count is assumed small.
func (r *NodePoolReconciler) poolsForMachine(ctx context.Context, obj client.Object) []reconcile.Request {
	machine, ok := obj.(*v1alpha1.Machine)
	if !ok {
		return nil
	}

	var pools v1alpha1.NodePoolList
	if err := r.List(ctx, &pools); err != nil {
		log.FromContext(ctx).Error(err, "list nodepools for machine", "machine", machine.Name)
		return nil
	}

	machineLabels := labels.Set(machine.Labels)
	requests := make([]reconcile.Request, 0, len(pools.Items))
	for i := range pools.Items {
		selector, err := metav1.LabelSelectorAsSelector(&pools.Items[i].Spec.MachineSelector)
		if err != nil {
			// Skip pools with a bad selector here; their own reconcile reports it.
			continue
		}
		if selector.Matches(machineLabels) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: pools.Items[i].Name},
			})
		}
	}
	return requests
}
