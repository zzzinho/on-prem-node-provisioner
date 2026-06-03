package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

// scaleUpBase is the fixed wall-clock instant the fake clock starts at, so
// cooldown math in tests is deterministic and easy to read.
var scaleUpBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

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
		WithStatusSubresource(&v1alpha1.NodePool{}).
		WithObjects(objs...).
		Build()
	clk := clocktesting.NewFakePassiveClock(scaleUpBase)
	return &ScaleUpReconciler{Client: cl, Scheme: scheme, Recorder: rec, Clock: clk}, cl
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

// gpuPoolWithGuards builds the gpu pool with optional maxNodes / cooldown.scaleUp
// guards and a status.lastScaleUpTime anchor. A nil arg leaves that guard unset.
func gpuPoolWithGuards(maxNodes *int32, cooldown *time.Duration, lastScaleUp *time.Time) *v1alpha1.NodePool {
	pool := gpuPool()
	pool.Spec.MaxNodes = maxNodes
	if cooldown != nil {
		pool.Spec.Cooldown.ScaleUp = &metav1.Duration{Duration: *cooldown}
	}
	if lastScaleUp != nil {
		t := metav1.NewTime(*lastScaleUp)
		pool.Status.LastScaleUpTime = &t
	}
	return pool
}

func int32Ptr(v int32) *int32               { return &v }
func durPtr(d time.Duration) *time.Duration { return &d }
func timePtr(t time.Time) *time.Time        { return &t }

// TestScaleUpGuards covers the M3.3 per-pool guards: maxNodes caps powered-on
// nodes and cooldown.scaleUp rate-limits successive wakes within a pool.
func TestScaleUpGuards(t *testing.T) {
	t.Parallel()

	gpu := map[string]string{"onp.io/pool": "gpu"}

	tests := []struct {
		name             string
		pool             *v1alpha1.NodePool
		machines         []client.Object
		wantWoken        []string
		wantRequeue      time.Duration // 0 means do not assert an exact value
		wantBlockedEvent bool          // ScaleUpBlocked Warning on the Pod
	}{
		{
			name: "maxNodes at cap: off member not woken",
			pool: gpuPoolWithGuards(int32Ptr(1), nil, nil),
			machines: []client.Object{
				scaleMachine("ready", gpu, "4", "8Gi", v1alpha1.MachineStateReady),
				scaleMachine("idle", gpu, "4", "8Gi", v1alpha1.MachineStateOff),
			},
			wantWoken:        nil,
			wantBlockedEvent: true,
		},
		{
			name: "maxNodes under cap: off member woken",
			pool: gpuPoolWithGuards(int32Ptr(2), nil, nil),
			machines: []client.Object{
				scaleMachine("ready", gpu, "4", "8Gi", v1alpha1.MachineStateReady),
				scaleMachine("idle", gpu, "4", "8Gi", v1alpha1.MachineStateOff),
			},
			wantWoken: []string{"idle"},
		},
		{
			name: "cooldown active: off member not woken, requeue at expiry",
			pool: gpuPoolWithGuards(nil, durPtr(5*time.Minute), timePtr(scaleUpBase.Add(-1*time.Minute))),
			machines: []client.Object{
				scaleMachine("idle", gpu, "4", "8Gi", v1alpha1.MachineStateOff),
			},
			wantWoken:   nil,
			wantRequeue: 4 * time.Minute,
		},
		{
			name: "cooldown elapsed: off member woken",
			pool: gpuPoolWithGuards(nil, durPtr(5*time.Minute), timePtr(scaleUpBase.Add(-6*time.Minute))),
			machines: []client.Object{
				scaleMachine("idle", gpu, "4", "8Gi", v1alpha1.MachineStateOff),
			},
			wantWoken: []string{"idle"},
		},
		{
			name: "nil maxNodes and nil cooldown: unchanged M3.2 wake",
			pool: gpuPoolWithGuards(nil, nil, nil),
			machines: []client.Object{
				scaleMachine("idle", gpu, "4", "8Gi", v1alpha1.MachineStateOff),
			},
			wantWoken: []string{"idle"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := record.NewFakeRecorder(16)
			objs := append([]client.Object{tt.pool, pendingPod("1", "1Gi", true)}, tt.machines...)
			r, cl := newScaleUpReconciler(t, rec, objs...)

			res, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: client.ObjectKey{Namespace: "default", Name: "p"},
			})
			if err != nil {
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

			if tt.wantRequeue != 0 && res.RequeueAfter != tt.wantRequeue {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tt.wantRequeue)
			}

			if got := blockedEventRecorded(rec); got != tt.wantBlockedEvent {
				t.Errorf("ScaleUpBlocked event = %v, want %v", got, tt.wantBlockedEvent)
			}
		})
	}
}

// blockedEventRecorded reports whether a ScaleUpBlocked Warning was emitted.
func blockedEventRecorded(rec *record.FakeRecorder) bool {
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, reasonScaleUpBlocked) {
				return true
			}
		default:
			return false
		}
	}
}

// TestScaleUpStampsCooldownOnWake verifies a successful wake stamps the winning
// pool's status.lastScaleUpTime to the clock's now, anchoring future cooldowns.
func TestScaleUpStampsCooldownOnWake(t *testing.T) {
	t.Parallel()

	gpu := map[string]string{"onp.io/pool": "gpu"}
	rec := record.NewFakeRecorder(16)
	pool := gpuPoolWithGuards(nil, durPtr(5*time.Minute), nil) // cooldown set, never scaled up yet
	r, cl := newScaleUpReconciler(t, rec,
		pool,
		pendingPod("1", "1Gi", true),
		scaleMachine("idle", gpu, "4", "8Gi", v1alpha1.MachineStateOff),
	)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: "p"},
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if woken := wokenMachines(t, cl); !woken["idle"] {
		t.Fatalf("machine %q not woken, want woken (woken=%v)", "idle", woken)
	}

	var got v1alpha1.NodePool
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "gpu"}, &got); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if got.Status.LastScaleUpTime == nil {
		t.Fatalf("LastScaleUpTime not stamped, want %v", scaleUpBase)
	}
	if !got.Status.LastScaleUpTime.Time.Equal(scaleUpBase) {
		t.Errorf("LastScaleUpTime = %v, want %v", got.Status.LastScaleUpTime.Time, scaleUpBase)
	}
}
