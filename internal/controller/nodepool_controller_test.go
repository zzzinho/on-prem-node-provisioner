package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

// poolMachine builds a Machine with the given name, labels and state for
// membership tests.
func poolMachine(name string, lbls map[string]string, state v1alpha1.MachineState) *v1alpha1.Machine {
	m := &v1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls},
		Spec: v1alpha1.MachineSpec{
			NodeName: name,
			Power:    v1alpha1.PowerSpec{Provider: "wol", WoL: &v1alpha1.WoLConfig{MacAddress: "aa:bb:cc:dd:ee:ff"}},
		},
	}
	m.Status.State = state
	return m
}

// gpuPool builds a NodePool selecting Machines labelled onp.io/pool=gpu.
func gpuPool() *v1alpha1.NodePool {
	return &v1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu"},
		Spec: v1alpha1.NodePoolSpec{
			MachineSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"onp.io/pool": "gpu"},
			},
		},
	}
}

func newPoolReconciler(t *testing.T, objs ...client.Object) (*NodePoolReconciler, client.Client) {
	t.Helper()
	scheme := newScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.NodePool{}).
		WithObjects(objs...).
		Build()
	return &NodePoolReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(16),
	}, cl
}

func TestNodePoolReconcileAggregatesMembership(t *testing.T) {
	t.Parallel()

	gpu := map[string]string{"onp.io/pool": "gpu"}
	cpu := map[string]string{"onp.io/pool": "cpu"}

	tests := []struct {
		name      string
		machines  []client.Object
		wantTotal int32
		wantReady int32
	}{
		{
			name: "counts only matching machines",
			machines: []client.Object{
				poolMachine("g1", gpu, v1alpha1.MachineStateOff),
				poolMachine("g2", gpu, v1alpha1.MachineStateBooting),
				poolMachine("c1", cpu, v1alpha1.MachineStateReady),
				poolMachine("c2", cpu, v1alpha1.MachineStateReady),
			},
			wantTotal: 2,
			wantReady: 0,
		},
		{
			name: "ready count reflects only Ready matching machines",
			machines: []client.Object{
				poolMachine("g1", gpu, v1alpha1.MachineStateReady),
				poolMachine("g2", gpu, v1alpha1.MachineStateReady),
				poolMachine("g3", gpu, v1alpha1.MachineStateBooting),
				poolMachine("c1", cpu, v1alpha1.MachineStateReady),
			},
			wantTotal: 3,
			wantReady: 2,
		},
		{
			name: "no matching machines yields zeros",
			machines: []client.Object{
				poolMachine("c1", cpu, v1alpha1.MachineStateReady),
			},
			wantTotal: 0,
			wantReady: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			objs := append([]client.Object{gpuPool()}, tt.machines...)
			r, cl := newPoolReconciler(t, objs...)

			if _, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: client.ObjectKey{Name: "gpu"},
			}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			var got v1alpha1.NodePool
			if err := cl.Get(context.Background(), client.ObjectKey{Name: "gpu"}, &got); err != nil {
				t.Fatalf("get nodepool: %v", err)
			}
			if got.Status.TotalMachines != tt.wantTotal {
				t.Errorf("TotalMachines = %d, want %d", got.Status.TotalMachines, tt.wantTotal)
			}
			if got.Status.ReadyMachines != tt.wantReady {
				t.Errorf("ReadyMachines = %d, want %d", got.Status.ReadyMachines, tt.wantReady)
			}
		})
	}
}

func TestPoolsForMachineMatchesByLabel(t *testing.T) {
	t.Parallel()

	r, _ := newPoolReconciler(t, gpuPool())

	tests := []struct {
		name     string
		labels   map[string]string
		wantPool bool
	}{
		{name: "matching labels enqueue the pool", labels: map[string]string{"onp.io/pool": "gpu"}, wantPool: true},
		{name: "non-matching labels enqueue nothing", labels: map[string]string{"onp.io/pool": "cpu"}, wantPool: false},
		{name: "no labels enqueue nothing", labels: nil, wantPool: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reqs := r.poolsForMachine(context.Background(), poolMachine("m", tt.labels, v1alpha1.MachineStateOff))
			if got := len(reqs) == 1 && reqs[0].Name == "gpu"; got != tt.wantPool {
				t.Errorf("poolsForMachine matched gpu = %v, want %v (reqs=%v)", got, tt.wantPool, reqs)
			}
		})
	}
}
