package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

// This file maps watch events on other objects to the Machines they affect.
// Each mapper answers "which Machines must re-reconcile"; the reconcilers decide
// what to do.

// requestsForMachinesOnNode returns a request for every Machine whose
// spec.nodeName is nodeName, found through the IndexMachineNodeName index.
func requestsForMachinesOnNode(ctx context.Context, c client.Client, nodeName string) []reconcile.Request {
	if nodeName == "" {
		return nil
	}
	var machines v1alpha1.MachineList
	if err := c.List(ctx, &machines, client.MatchingFields{IndexMachineNodeName: nodeName}); err != nil {
		log.FromContext(ctx).Error(err, "list machines for node", "node", nodeName)
		return nil
	}
	return machineRequests(machines.Items)
}

// requestsForPoolMembers returns a request for every Machine the pool's
// machineSelector matches. A malformed selector maps to nothing; the pool's own
// reconcile reports it.
func requestsForPoolMembers(ctx context.Context, c client.Client, pool *v1alpha1.NodePool) []reconcile.Request {
	selector, err := metav1.LabelSelectorAsSelector(&pool.Spec.MachineSelector)
	if err != nil {
		return nil
	}
	var machines v1alpha1.MachineList
	if err := c.List(ctx, &machines, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		log.FromContext(ctx).Error(err, "list machines for pool", "pool", pool.Name)
		return nil
	}
	return machineRequests(machines.Items)
}

func machineRequests(machines []v1alpha1.Machine) []reconcile.Request {
	requests := make([]reconcile.Request, 0, len(machines))
	for i := range machines {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: machines[i].Name},
		})
	}
	return requests
}
