package controller

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

// TestStateCollectorNodesTotal: Machines are counted by their first matching pool
// and state; a Machine matching no pool lands under "<none>".
func TestStateCollectorNodesTotal(t *testing.T) {
	scheme := newScheme(t)
	pool := nodePool("edge", sdLabels, nil)
	ready := sdMachine("a", v1alpha1.MachineStateReady, nil)
	off := sdMachine("b", v1alpha1.MachineStateOff, nil)
	unpooled := sdMachine("c", v1alpha1.MachineStateReady, nil)
	unpooled.Labels = map[string]string{"pool": "other"} // matches no pool

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pool, ready, off, unpooled).Build()
	c := NewStateCollector(cl)

	expected := `
# HELP onp_nodes_total Number of Machines by pool and lifecycle state.
# TYPE onp_nodes_total gauge
onp_nodes_total{pool="edge",state="Ready"} 1
onp_nodes_total{pool="edge",state="Off"} 1
onp_nodes_total{pool="<none>",state="Ready"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "onp_nodes_total"); err != nil {
		t.Error(err)
	}
}

// TestStateCollectorPendingUnschedulable: only unbound, Pending, Unschedulable pods
// are counted.
func TestStateCollectorPendingUnschedulable(t *testing.T) {
	scheme := newScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(unschedulablePod("want-node"), scheduledPod("bound", "node-a")).Build()
	c := NewStateCollector(cl)

	expected := `
# HELP onp_pending_unschedulable Number of unschedulable pending pods awaiting a node.
# TYPE onp_pending_unschedulable gauge
onp_pending_unschedulable 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "onp_pending_unschedulable"); err != nil {
		t.Error(err)
	}
}

// unschedulablePod returns a pending pod the scheduler marked Unschedulable.
func unschedulablePod(name string) client.Object {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodScheduled,
				Status: corev1.ConditionFalse,
				Reason: corev1.PodReasonUnschedulable,
			}},
		},
	}
}

// scheduledPod returns a pod already bound to a node.
func scheduledPod(name, node string) client.Object {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}
