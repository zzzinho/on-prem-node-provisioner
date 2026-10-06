package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

// This file holds the safety predicates the reconcilers consult before a power
// action. Each answers one question about the cluster and decides nothing on its
// own; the reconcilers choose the transition.

// nodeAlwaysOn reports whether the named Node carries the always-on label. A
// missing Node is not always-on: there is nothing ONP could power off.
func nodeAlwaysOn(ctx context.Context, c client.Client, nodeName string) (bool, error) {
	if nodeName == "" {
		return false, nil
	}
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get node %q for always-on check: %w", nodeName, err)
	}
	return node.Labels[v1alpha1.LabelAlwaysOn] == v1alpha1.LabelAlwaysOnValue, nil
}

// otherMachinesOnNode returns the names of the other Machines that claim the
// same Node. Two Machines on one Node drive one host from two state machines: a
// wake of one reads as the other's Node going Ready, and a power-off of one takes
// down a node the other still counts as on.
func otherMachinesOnNode(ctx context.Context, c client.Client, m *v1alpha1.Machine) ([]string, error) {
	if m.Spec.NodeName == "" {
		return nil, nil
	}
	var machines v1alpha1.MachineList
	if err := c.List(ctx, &machines, client.MatchingFields{IndexMachineNodeName: m.Spec.NodeName}); err != nil {
		return nil, fmt.Errorf("list machines on node %q: %w", m.Spec.NodeName, err)
	}
	var others []string
	for i := range machines.Items {
		if machines.Items[i].Name != m.Name {
			others = append(others, machines.Items[i].Name)
		}
	}
	return others, nil
}
