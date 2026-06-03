package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
	"github.com/zzzinho/on-prem-node-provisioner/internal/power"
)

// fakeProvider records PowerOn calls and can inject an error. It advertises the
// CanPowerOn capability so the reconciler proceeds past the gate.
type fakeProvider struct {
	powerOnCalls int
	powerOnErr   error
}

func (p *fakeProvider) Name() string { return "wol" }

func (p *fakeProvider) Capabilities() power.Capabilities {
	return power.Capabilities{CanPowerOn: true}
}

func (p *fakeProvider) PowerOn(context.Context, *v1alpha1.Machine) error {
	p.powerOnCalls++
	return p.powerOnErr
}

func (p *fakeProvider) PowerOff(context.Context, *v1alpha1.Machine) error {
	return power.ErrUnsupported
}

func (p *fakeProvider) PowerStatus(context.Context, *v1alpha1.Machine) (v1alpha1.MachineState, error) {
	return "", power.ErrUnsupported
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add v1alpha1 to scheme: %v", err)
	}
	return s
}

// machine builds a wol Machine in the given state with the given annotations.
func machine(state v1alpha1.MachineState, annotations map[string]string) *v1alpha1.Machine {
	m := &v1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node-a",
			Annotations: annotations,
		},
		Spec: v1alpha1.MachineSpec{
			NodeName: "node-a",
			Power: v1alpha1.PowerSpec{
				Provider: "wol",
				WoL:      &v1alpha1.WoLConfig{MacAddress: "aa:bb:cc:dd:ee:ff"},
			},
		},
	}
	m.Status.State = state
	return m
}

// readyNode returns a Node with Ready=True.
func readyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// notReadyNode returns a Node with Ready=False, as a powered-off node reports
// once the kubelet stops heartbeating.
func notReadyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	}
}

type reconcilerFixture struct {
	r        *MachineReconciler
	cl       client.Client
	provider *fakeProvider
	clock    *clocktesting.FakeClock
}

func newFixture(t *testing.T, objs ...client.Object) *reconcilerFixture {
	t.Helper()
	scheme := newScheme(t)
	provider := &fakeProvider{}
	registry := power.NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatalf("register provider: %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Machine{}).
		WithObjects(objs...).
		Build()

	fc := clocktesting.NewFakeClock(time.Now())
	return &reconcilerFixture{
		r: &MachineReconciler{
			Client:      cl,
			Scheme:      scheme,
			Registry:    registry,
			BootTimeout: 10 * time.Minute,
			Recorder:    record.NewFakeRecorder(16),
			Clock:       fc,
		},
		cl:       cl,
		provider: provider,
		clock:    fc,
	}
}

func (f *reconcilerFixture) reconcile(t *testing.T) reconcile.Result {
	t.Helper()
	res, err := f.r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "node-a"},
	})
	if err != nil {
		t.Fatalf("Reconcile() unexpected error: %v", err)
	}
	return res
}

func (f *reconcilerFixture) getMachine(t *testing.T) *v1alpha1.Machine {
	t.Helper()
	var m v1alpha1.Machine
	if err := f.cl.Get(context.Background(), types.NamespacedName{Name: "node-a"}, &m); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	return &m
}

// condition returns the named status condition, or nil when absent.
func condition(m *v1alpha1.Machine, condType string) *metav1.Condition {
	return meta.FindStatusCondition(m.Status.Conditions, condType)
}

func TestReconcileOffWithWakeAnnotationPowersOn(t *testing.T) {
	t.Parallel()

	f := newFixture(t, machine(v1alpha1.MachineStateOff, map[string]string{
		v1alpha1.AnnotationWakeNow: v1alpha1.AnnotationWakeNowValue,
	}))

	f.reconcile(t)

	if f.provider.powerOnCalls != 1 {
		t.Fatalf("PowerOn calls = %d, want 1", f.provider.powerOnCalls)
	}
	m := f.getMachine(t)
	if m.Status.State != v1alpha1.MachineStateBooting {
		t.Errorf("state = %q, want %q", m.Status.State, v1alpha1.MachineStateBooting)
	}
	if m.Status.BootStartTime == nil {
		t.Error("BootStartTime = nil, want set")
	}
}

func TestReconcileBootingNodeReadyBecomesReady(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateBooting, map[string]string{
		v1alpha1.AnnotationWakeNow: v1alpha1.AnnotationWakeNowValue,
	})
	start := metav1.Now()
	m.Status.BootStartTime = &start

	f := newFixture(t, m, readyNode("node-a"))

	f.reconcile(t)

	got := f.getMachine(t)
	if got.Status.State != v1alpha1.MachineStateReady {
		t.Errorf("state = %q, want %q", got.Status.State, v1alpha1.MachineStateReady)
	}
	if _, ok := got.Annotations[v1alpha1.AnnotationWakeNow]; ok {
		t.Error("wake-now annotation still present, want removed")
	}
}

func TestReconcileBootingTimesOutFails(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateBooting, nil)
	f := newFixture(t, m)
	// Stamp BootStartTime at the fake clock's current time, then advance past
	// the boot timeout so the next reconcile observes the deadline crossed.
	start := metav1.NewTime(f.clock.Now())
	m.Status.BootStartTime = &start
	if err := f.cl.Status().Update(context.Background(), m); err != nil {
		t.Fatalf("seed BootStartTime: %v", err)
	}
	f.clock.Step(11 * time.Minute)

	f.reconcile(t)

	got := f.getMachine(t)
	if got.Status.State != v1alpha1.MachineStateFailed {
		t.Errorf("state = %q, want %q", got.Status.State, v1alpha1.MachineStateFailed)
	}
}

func TestReconcileOffPowerOnErrorStaysOff(t *testing.T) {
	t.Parallel()

	f := newFixture(t, machine(v1alpha1.MachineStateOff, map[string]string{
		v1alpha1.AnnotationWakeNow: v1alpha1.AnnotationWakeNowValue,
	}))
	f.provider.powerOnErr = errors.New("agent unreachable")

	// PowerOn error surfaces as a reconcile error (so controller-runtime
	// requeues with backoff); call Reconcile directly to assert on it.
	_, err := f.r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "node-a"},
	})
	if err == nil {
		t.Fatal("Reconcile() err = nil, want error on power-on failure")
	}

	m := f.getMachine(t)
	if m.Status.State != v1alpha1.MachineStateOff {
		t.Errorf("state = %q, want %q (must not advance on power-on failure)", m.Status.State, v1alpha1.MachineStateOff)
	}

	// A retried reconcile calls PowerOn again — the Machine is still Off and
	// still annotated.
	f.provider.powerOnErr = nil
	f.reconcile(t)
	if f.provider.powerOnCalls != 2 {
		t.Errorf("PowerOn calls = %d, want 2 (retry on next reconcile)", f.provider.powerOnCalls)
	}
	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateBooting {
		t.Errorf("state after retry = %q, want %q", got.Status.State, v1alpha1.MachineStateBooting)
	}
}

func TestReconcileShuttingDownNodeNotReadyBecomesOff(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateShuttingDown, nil)
	f := newFixture(t, m, notReadyNode("node-a"))

	res := f.reconcile(t)

	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 (node is gone, no need to poll)", res.RequeueAfter)
	}
	got := f.getMachine(t)
	if got.Status.State != v1alpha1.MachineStateOff {
		t.Errorf("state = %q, want %q", got.Status.State, v1alpha1.MachineStateOff)
	}
	cond := condition(got, v1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready condition = %+v, want status False", cond)
	}
}

func TestReconcileShuttingDownNodeStillReadyKeepsPolling(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateShuttingDown, nil)
	f := newFixture(t, m, readyNode("node-a"))

	res := f.reconcile(t)

	if res.RequeueAfter != shutdownPollInterval {
		t.Errorf("RequeueAfter = %v, want %v (poweroff not observed yet)", res.RequeueAfter, shutdownPollInterval)
	}
	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateShuttingDown {
		t.Errorf("state = %q, want %q (must stay until Node goes NotReady)", got.Status.State, v1alpha1.MachineStateShuttingDown)
	}
}

func TestReconcileUnsetStateInitializesToOff(t *testing.T) {
	t.Parallel()

	f := newFixture(t, machine("", nil))

	res := f.reconcile(t)

	if !res.Requeue {
		t.Error("Requeue = false, want true after initialize")
	}
	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateOff {
		t.Errorf("state = %q, want %q", got.Status.State, v1alpha1.MachineStateOff)
	}
}
