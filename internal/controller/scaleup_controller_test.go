package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

// scaleMachine builds a pool-member Machine with a CPU/memory capacity and state
// for scale-up tests. Labels place it in a pool; capacity drives the fit check
// and best-fit ordering.
func scaleMachine(name string, lbls map[string]string, cpu, mem string, state v1alpha1.MachineState) *v1alpha1.Machine {
	m := &v1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls},
		Spec: v1alpha1.MachineSpec{
			NodeName: name,
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
			Power: v1alpha1.PowerSpec{
				Provider: "wol",
				WoL:      &v1alpha1.WoLConfig{MacAddress: "aa:bb:cc:dd:ee:ff"},
			},
		},
	}
	m.Status.State = state
	return m
}

// pendingPod builds an unbound Pending Pod requesting cpu/mem. When unsched is
// true it carries the PodScheduled=False/Unschedulable condition that marks it a
// scale-up candidate.
func pendingPod(cpu, mem string, unsched bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "c",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(cpu),
						corev1.ResourceMemory: resource.MustParse(mem),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	if unsched {
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodScheduled,
			Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable,
		}}
	}
	return pod
}

func newScaleUpReconciler(t *testing.T, rec record.EventRecorder, objs ...client.Object) (*ScaleUpReconciler, client.Client) {
	t.Helper()
	scheme := newScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
	return &ScaleUpReconciler{Client: cl, Scheme: scheme, Recorder: rec}, cl
}

// wokenMachines returns the names of Machines carrying the wake-now trigger.
func wokenMachines(t *testing.T, cl client.Client) map[string]bool {
	t.Helper()
	var list v1alpha1.MachineList
	if err := cl.List(context.Background(), &list); err != nil {
		t.Fatalf("list machines: %v", err)
	}
	woken := map[string]bool{}
	for i := range list.Items {
		if list.Items[i].Annotations[v1alpha1.AnnotationWakeNow] == v1alpha1.AnnotationWakeNowValue {
			woken[list.Items[i].Name] = true
		}
	}
	return woken
}

func TestScaleUpReconcileWakesBestFitMachine(t *testing.T) {
	t.Parallel()

	gpu := map[string]string{"onp.io/pool": "gpu"}
	cpuLbl := map[string]string{"onp.io/pool": "cpu"}

	tests := []struct {
		name      string
		pod       *corev1.Pod
		machines  []client.Object
		wantWoken []string // exact set of machines expected to carry wake-now
		wantEvent bool
	}{
		{
			name: "one off fitting machine is woken",
			pod:  pendingPod("1", "1Gi", true),
			machines: []client.Object{
				scaleMachine("g1", gpu, "4", "8Gi", v1alpha1.MachineStateOff),
			},
			wantWoken: []string{"g1"},
			wantEvent: true,
		},
		{
			name: "smaller of two fitting machines is woken (best-fit)",
			pod:  pendingPod("1", "1Gi", true),
			machines: []client.Object{
				scaleMachine("big", gpu, "16", "64Gi", v1alpha1.MachineStateOff),
				scaleMachine("small", gpu, "4", "8Gi", v1alpha1.MachineStateOff),
			},
			wantWoken: []string{"small"},
			wantEvent: true,
		},
		{
			name: "too-small machine is not woken; nothing fits",
			pod:  pendingPod("8", "1Gi", true),
			machines: []client.Object{
				scaleMachine("tiny", gpu, "2", "8Gi", v1alpha1.MachineStateOff),
			},
			wantWoken: nil,
			wantEvent: false,
		},
		{
			name: "in-flight booting fit machine blocks waking an idle off fit machine",
			pod:  pendingPod("1", "1Gi", true),
			machines: []client.Object{
				scaleMachine("booting", gpu, "4", "8Gi", v1alpha1.MachineStateBooting),
				scaleMachine("idle", gpu, "4", "8Gi", v1alpha1.MachineStateOff),
			},
			wantWoken: nil,
			wantEvent: false,
		},
		{
			name: "machine outside any pool selector is not considered",
			pod:  pendingPod("1", "1Gi", true),
			machines: []client.Object{
				scaleMachine("orphan", cpuLbl, "4", "8Gi", v1alpha1.MachineStateOff),
			},
			wantWoken: nil,
			wantEvent: false,
		},
		{
			name:      "pending pod without unschedulable condition is a no-op",
			pod:       pendingPod("1", "1Gi", false),
			machines:  []client.Object{scaleMachine("g1", gpu, "4", "8Gi", v1alpha1.MachineStateOff)},
			wantWoken: nil,
			wantEvent: false,
		},
		{
			name: "already bound pod is a no-op",
			pod: func() *corev1.Pod {
				p := pendingPod("1", "1Gi", true)
				p.Spec.NodeName = "node-x"
				return p
			}(),
			machines:  []client.Object{scaleMachine("g1", gpu, "4", "8Gi", v1alpha1.MachineStateOff)},
			wantWoken: nil,
			wantEvent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := record.NewFakeRecorder(16)
			objs := append([]client.Object{gpuPool(), tt.pod}, tt.machines...)
			r, cl := newScaleUpReconciler(t, rec, objs...)

			if _, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: client.ObjectKey{Namespace: "default", Name: "p"},
			}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			woken := wokenMachines(t, cl)
			want := map[string]bool{}
			for _, n := range tt.wantWoken {
				want[n] = true
			}
			if len(woken) != len(want) {
				t.Fatalf("woken machines = %v, want %v", woken, want)
			}
			for n := range want {
				if !woken[n] {
					t.Errorf("machine %q not woken, want woken (woken=%v)", n, woken)
				}
			}

			gotEvent := len(rec.Events) > 0
			if gotEvent != tt.wantEvent {
				t.Errorf("event recorded = %v, want %v", gotEvent, tt.wantEvent)
			}
		})
	}
}
