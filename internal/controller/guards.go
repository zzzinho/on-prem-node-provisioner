package controller

import (
	"context"
	"fmt"
	"time"

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

// reservedNodeLabels are Node labels only an operator sets. ONP reads them as
// safety signals, so a NodePool template or Machine label must not be able to
// write — and so silently flip — them.
var reservedNodeLabels = []string{v1alpha1.LabelAlwaysOn}

// dropReservedLabels deletes the reserved keys from labels in place and returns
// the keys it removed, in reservedNodeLabels order.
func dropReservedLabels(labels map[string]string) []string {
	var dropped []string
	for _, k := range reservedNodeLabels {
		if _, ok := labels[k]; ok {
			delete(labels, k)
			dropped = append(dropped, k)
		}
	}
	return dropped
}

// nodeWentDownSince reports whether the named Node has been seen going NotReady
// at or after since — the evidence that a power-off issued at since landed. A
// Node already NotReady before since proves nothing about this power-off (a
// network partition, a kubelet that was already down), so it does not count, and
// neither does a Node with no Ready condition yet. A missing Node counts as down:
// there is no host left to wait for. A zero since accepts any NotReady.
func nodeWentDownSince(ctx context.Context, c client.Client, nodeName string, since time.Time) (bool, error) {
	if nodeName == "" {
		return true, nil
	}
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("get node %q for power-off check: %w", nodeName, err)
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type != corev1.NodeReady {
			continue
		}
		return cond.Status != corev1.ConditionTrue && !cond.LastTransitionTime.Time.Before(since), nil
	}
	return false, nil
}

// nothingKeepsNodeOn reports, reading through reader, whether no pod that keeps
// the node on (by keepsNodeOn) remains on it. The drain hands a node to the
// power-off leg on this answer, so the caller passes an uncached reader.
func nothingKeepsNodeOn(ctx context.Context, reader client.Reader, nodeName string) (bool, error) {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.MatchingFields{IndexPodNodeName: nodeName}); err != nil {
		return false, fmt.Errorf("list pods on node %q: %w", nodeName, err)
	}
	for i := range pods.Items {
		if keepsNodeOn(&pods.Items[i]) {
			return false, nil
		}
	}
	return true, nil
}

// nodeIsReady reports whether the named Node's Ready condition is True. A missing
// Node is not Ready.
func nodeIsReady(ctx context.Context, c client.Client, nodeName string) (bool, error) {
	if nodeName == "" {
		return false, nil
	}
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue, nil
		}
	}
	return false, nil
}
