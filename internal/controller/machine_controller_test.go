package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

func (p *fakeProvider) PowerStatus(context.Context, *v1alpha1.Machine) (power.State, error) {
	return power.StateUnknown, power.ErrUnsupported
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
	// evicted records the pods passed to the stubbed Evict, in call order.
	evicted []string
	// evictErr, when set, is returned by the stubbed Evict for every pod.
	evictErr error
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
		// Mirror main.go's pod index so reconcileDraining's MatchingFields list
		// resolves a node's pods under the fake client.
		WithIndex(&corev1.Pod{}, IndexPodNodeName, func(o client.Object) []string {
			pod, ok := o.(*corev1.Pod)
			if !ok || pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}).
		// Mirror main.go's Machine index so the duplicate-Node guard resolves.
		WithIndex(&v1alpha1.Machine{}, IndexMachineNodeName, func(o client.Object) []string {
			m, ok := o.(*v1alpha1.Machine)
			if !ok || m.Spec.NodeName == "" {
				return nil
			}
			return []string{m.Spec.NodeName}
		}).
		WithObjects(objs...).
		Build()

	fc := clocktesting.NewFakeClock(time.Now())
	f := &reconcilerFixture{
		cl:       cl,
		provider: provider,
		clock:    fc,
	}
	f.r = &MachineReconciler{
		Client:              cl,
		Scheme:              scheme,
		Registry:            registry,
		BootTimeout:         10 * time.Minute,
		ShutdownTimeout:     5 * time.Minute,
		NodeLossGracePeriod: time.Minute,
		Recorder:            record.NewFakeRecorder(16),
		Clock:               fc,
		APIReader:           cl,
		// Stub Evict: the fake client's eviction subresource deletes the pod
		// unconditionally and never returns the PDB-blocked TooManyRequests we
		// must exercise, so the test drives eviction through this stub.
		Evict: func(_ context.Context, pod *corev1.Pod) error {
			f.evicted = append(f.evicted, pod.Name)
			return f.evictErr
		},
	}
	return f
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

// TestReconcileHoldsMachineSharingNode: when two Machines claim one Node, neither
// acts — a wake-now on one is not powered on — and a DuplicateNode Event says why.
func TestReconcileHoldsMachineSharingNode(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateOff, map[string]string{
		v1alpha1.AnnotationWakeNow: v1alpha1.AnnotationWakeNowValue,
	})
	dup := machine(v1alpha1.MachineStateOff, nil)
	dup.Name = "node-a-copy"
	f := newFixture(t, m, dup, notReadyNode("node-a"))

	res := f.reconcile(t)

	if f.provider.powerOnCalls != 0 {
		t.Errorf("PowerOn calls = %d, want 0 while another Machine claims the Node", f.provider.powerOnCalls)
	}
	if res.RequeueAfter != duplicateNodeRecheck {
		t.Errorf("RequeueAfter = %s, want %s so removing the duplicate releases the hold", res.RequeueAfter, duplicateNodeRecheck)
	}
	assertEvent(t, f.r.Recorder.(*record.FakeRecorder), reasonDuplicateNode)
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

// TestReconcileBootingResendsPowerOn: while the Node is not yet Ready, each poll
// past the first re-sends the power-on, so one lost WoL packet (sent while the
// board was still halting) does not run the boot out into Failed.
func TestReconcileBootingResendsPowerOn(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateBooting, nil)
	start := metav1.Now()
	m.Status.BootStartTime = &start
	f := newFixture(t, m, notReadyNode("node-a"))

	f.reconcile(t)
	if f.provider.powerOnCalls != 0 {
		t.Fatalf("PowerOn calls = %d, want 0 within the first poll interval", f.provider.powerOnCalls)
	}

	f.clock.Step(bootPollInterval)
	f.reconcile(t)
	if f.provider.powerOnCalls != 1 {
		t.Fatalf("PowerOn calls = %d, want 1 re-send once a poll interval has passed", f.provider.powerOnCalls)
	}
	if got := f.getMachine(t).Status.State; got != v1alpha1.MachineStateBooting {
		t.Errorf("state = %q, want %q (a re-send does not change state)", got, v1alpha1.MachineStateBooting)
	}
}

func (f *reconcilerFixture) getNode(t *testing.T, name string) *corev1.Node {
	t.Helper()
	var n corev1.Node
	if err := f.cl.Get(context.Background(), types.NamespacedName{Name: name}, &n); err != nil {
		t.Fatalf("get node %q: %v", name, err)
	}
	return &n
}

// onpCordonedReadyNode is a Ready Node that ONP cordoned during a prior
// scale-down: unschedulable and carrying the onp.io/cordoned-by-onp marker, as
// such a node looks once it is powered back on.
func onpCordonedReadyNode(name string) *corev1.Node {
	n := readyNode(name)
	n.Spec.Unschedulable = true
	n.Annotations = map[string]string{v1alpha1.AnnotationCordonedByONP: "true"}
	return n
}

// TestReconcileBootingUncordonsONPCordonedNode: a node ONP cordoned during a
// prior scale-down is uncordoned (and the marker cleared) when it is woken back
// to Ready, so it can host pods again.
// TestReconcileBootingKeepsReservedLabel: a Machine label cannot write the
// operator-only always-on label onto the Node — the Node keeps its own value, the
// other labels still apply, and a ReservedLabel Event names the dropped key.
func TestReconcileBootingKeepsReservedLabel(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateBooting, nil)
	start := metav1.Now()
	m.Status.BootStartTime = &start
	m.Spec.Labels = map[string]string{v1alpha1.LabelAlwaysOn: "false", "team": "a"}
	node := readyNode("node-a")
	node.Labels = map[string]string{v1alpha1.LabelAlwaysOn: v1alpha1.LabelAlwaysOnValue}
	f := newFixture(t, m, node)

	f.reconcile(t)

	labels := f.getNode(t, "node-a").Labels
	if labels[v1alpha1.LabelAlwaysOn] != v1alpha1.LabelAlwaysOnValue {
		t.Errorf("always-on label = %q, want the operator's %q kept", labels[v1alpha1.LabelAlwaysOn], v1alpha1.LabelAlwaysOnValue)
	}
	if labels["team"] != "a" {
		t.Errorf("team label = %q, want %q applied", labels["team"], "a")
	}
	assertEvent(t, f.r.Recorder.(*record.FakeRecorder), reasonReservedLabel)
}

func TestReconcileBootingUncordonsONPCordonedNode(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateBooting, map[string]string{
		v1alpha1.AnnotationWakeNow: v1alpha1.AnnotationWakeNowValue,
	})
	start := metav1.Now()
	m.Status.BootStartTime = &start

	f := newFixture(t, m, onpCordonedReadyNode("node-a"))

	f.reconcile(t)

	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateReady {
		t.Fatalf("state = %q, want Ready", got.Status.State)
	}
	n := f.getNode(t, "node-a")
	if n.Spec.Unschedulable {
		t.Error("node still cordoned, want uncordoned on wake")
	}
	if _, ok := n.Annotations[v1alpha1.AnnotationCordonedByONP]; ok {
		t.Error("cordoned-by-onp marker still present, want removed")
	}
}

// TestReconcileBootingLeavesOperatorCordonAlone: a node an operator cordoned by
// hand (no onp.io/cordoned-by-onp marker) stays cordoned when ONP wakes it — ONP
// uncordons only its own cordons.
func TestReconcileBootingLeavesOperatorCordonAlone(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateBooting, map[string]string{
		v1alpha1.AnnotationWakeNow: v1alpha1.AnnotationWakeNowValue,
	})
	start := metav1.Now()
	m.Status.BootStartTime = &start

	node := readyNode("node-a")
	node.Spec.Unschedulable = true // operator-cordoned: no onp marker

	f := newFixture(t, m, node)

	f.reconcile(t)

	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateReady {
		t.Fatalf("state = %q, want Ready", got.Status.State)
	}
	if n := f.getNode(t, "node-a"); !n.Spec.Unschedulable {
		t.Error("operator cordon was lifted, want left in place")
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

// TestReconcileOffAdoptsReadyNode: a Machine that reads Off while its Node is
// Ready (back from a node loss, powered on by hand, or created over a running
// node) is adopted straight into Ready — no power-on — and the Node gets its
// template.
func TestReconcileOffAdoptsReadyNode(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateOff, nil)
	m.Spec.Labels = map[string]string{"team": "a"}
	f := newFixture(t, m, readyNode("node-a"))

	f.reconcile(t)

	if got := f.getMachine(t).Status.State; got != v1alpha1.MachineStateReady {
		t.Errorf("state = %q, want %q", got, v1alpha1.MachineStateReady)
	}
	if f.provider.powerOnCalls != 0 {
		t.Errorf("PowerOn calls = %d, want 0 for a node that is already up", f.provider.powerOnCalls)
	}
	if got := f.getNode(t, "node-a").Labels["team"]; got != "a" {
		t.Errorf("team label = %q, want the template applied", got)
	}
	assertEvent(t, f.r.Recorder.(*record.FakeRecorder), reasonAdopted)
}

// TestReconcileBootTimeoutClearsWakeNow: a boot that times out spends its wake
// request, so an operator returning the Failed Machine to Off does not power the
// node straight back on.
func TestReconcileBootTimeoutClearsWakeNow(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateBooting, map[string]string{
		v1alpha1.AnnotationWakeNow: v1alpha1.AnnotationWakeNowValue,
	})
	f := newFixture(t, m, notReadyNode("node-a"))
	start := metav1.NewTime(f.clock.Now())
	m.Status.BootStartTime = &start
	if err := f.cl.Status().Update(context.Background(), m); err != nil {
		t.Fatalf("seed BootStartTime: %v", err)
	}
	f.clock.Step(11 * time.Minute)

	f.reconcile(t)

	got := f.getMachine(t)
	if got.Status.State != v1alpha1.MachineStateFailed {
		t.Fatalf("state = %q, want %q", got.Status.State, v1alpha1.MachineStateFailed)
	}
	if _, ok := got.Annotations[v1alpha1.AnnotationWakeNow]; ok {
		t.Error("wake-now kept on a Failed Machine, want removed")
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

// TestReconcileShuttingDownTimesOutFails (A1): a node that never goes NotReady
// after power-off must not poll forever — once the shutdown budget elapses the
// Machine is failed.
// notReadyNodeSince returns a Node whose Ready condition turned False at at.
func notReadyNodeSince(name string, at time.Time) *corev1.Node {
	n := notReadyNode(name)
	n.Status.Conditions[0].LastTransitionTime = metav1.NewTime(at)
	return n
}

// TestReconcileShuttingDownOffOnceNodeGoesDown: a Node that turns NotReady after
// the power-off was issued is the evidence it landed — the Machine goes Off.
func TestReconcileShuttingDownOffOnceNodeGoesDown(t *testing.T) {
	t.Parallel()

	issued := time.Now().Truncate(time.Second)
	m := machine(v1alpha1.MachineStateShuttingDown, nil)
	m.Status.ShutdownStartTime = &metav1.Time{Time: issued}
	f := newFixture(t, m, notReadyNodeSince("node-a", issued.Add(40*time.Second)))

	f.reconcile(t)

	if got := f.getMachine(t).Status.State; got != v1alpha1.MachineStateOff {
		t.Errorf("state = %q, want %q", got, v1alpha1.MachineStateOff)
	}
}

// TestReconcileShuttingDownIgnoresEarlierNotReady: a Node that was already
// NotReady before the power-off was issued (cut off while running) does not prove
// the power-off landed — the Machine keeps waiting and fails at the shutdown
// timeout instead of reporting a power-off that may not have happened.
func TestReconcileShuttingDownIgnoresEarlierNotReady(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateShuttingDown, nil)
	f := newFixture(t, m, notReadyNodeSince("node-a", time.Now().Add(-time.Minute)))
	start := metav1.NewTime(f.clock.Now())
	m.Status.ShutdownStartTime = &start
	if err := f.cl.Status().Update(context.Background(), m); err != nil {
		t.Fatalf("seed ShutdownStartTime: %v", err)
	}

	res := f.reconcile(t)
	if got := f.getMachine(t).Status.State; got != v1alpha1.MachineStateShuttingDown {
		t.Fatalf("state = %q, want %q while the power-off is unconfirmed", got, v1alpha1.MachineStateShuttingDown)
	}
	if res.RequeueAfter == 0 {
		t.Error("RequeueAfter = 0, want a poll while waiting")
	}

	f.clock.Step(6 * time.Minute)
	f.reconcile(t)
	if got := f.getMachine(t).Status.State; got != v1alpha1.MachineStateFailed {
		t.Errorf("state = %q, want %q at the shutdown timeout", got, v1alpha1.MachineStateFailed)
	}
}

func TestReconcileShuttingDownTimesOutFails(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateShuttingDown, nil)
	f := newFixture(t, m, readyNode("node-a"))
	// Stamp ShutdownStartTime at the fake clock, then advance past the timeout
	// while the node stays Ready (the power-off never landed).
	start := metav1.NewTime(f.clock.Now())
	m.Status.ShutdownStartTime = &start
	if err := f.cl.Status().Update(context.Background(), m); err != nil {
		t.Fatalf("seed ShutdownStartTime: %v", err)
	}
	f.clock.Step(6 * time.Minute)

	f.reconcile(t)

	got := f.getMachine(t)
	if got.Status.State != v1alpha1.MachineStateFailed {
		t.Errorf("state = %q, want %q", got.Status.State, v1alpha1.MachineStateFailed)
	}
	if got.Status.ShutdownStartTime != nil {
		t.Error("ShutdownStartTime still set, want cleared on Failed")
	}
	cond := condition(got, v1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ShutdownTimeout" {
		t.Errorf("Ready condition = %+v, want False/ShutdownTimeout", cond)
	}
}

// TestReconcileReadyNodeNotReadyStampsAnchor (A2): a Ready Machine whose Node goes
// NotReady (with no ONP drain) anchors the loss timer and waits out the grace
// window rather than dropping to Off on a possibly-transient blip.
func TestReconcileReadyNodeNotReadyStampsAnchor(t *testing.T) {
	t.Parallel()

	f := newFixture(t, machine(v1alpha1.MachineStateReady, nil), notReadyNode("node-a"))

	res := f.reconcile(t)

	if res.RequeueAfter != f.r.NodeLossGracePeriod {
		t.Errorf("RequeueAfter = %v, want %v (grace window)", res.RequeueAfter, f.r.NodeLossGracePeriod)
	}
	got := f.getMachine(t)
	if got.Status.State != v1alpha1.MachineStateReady {
		t.Errorf("state = %q, want Ready (within grace)", got.Status.State)
	}
	if got.Status.NotReadySince == nil {
		t.Error("NotReadySince = nil, want stamped")
	}
}

// TestReconcileReadyNodeNotReadyPastGraceBecomesOff (A2): once the Node has stayed
// NotReady for the whole grace window the Machine falls back to Off so scale-up
// can wake it again.
func TestReconcileReadyNodeNotReadyPastGraceBecomesOff(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateReady, nil)
	f := newFixture(t, m, notReadyNode("node-a"))
	start := metav1.NewTime(f.clock.Now())
	m.Status.NotReadySince = &start
	if err := f.cl.Status().Update(context.Background(), m); err != nil {
		t.Fatalf("seed NotReadySince: %v", err)
	}
	f.clock.Step(61 * time.Second)

	f.reconcile(t)

	got := f.getMachine(t)
	if got.Status.State != v1alpha1.MachineStateOff {
		t.Errorf("state = %q, want Off (node lost past grace)", got.Status.State)
	}
	if got.Status.NotReadySince != nil {
		t.Error("NotReadySince still set, want cleared on Off")
	}
	cond := condition(got, v1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "NodeLost" {
		t.Errorf("Ready condition = %+v, want False/NodeLost", cond)
	}
}

// TestReconcileReadyNodeRecoversClearsAnchor (A2): a Node that recovers within the
// grace window clears the loss anchor and the Machine stays Ready.
func TestReconcileReadyNodeRecoversClearsAnchor(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateReady, nil)
	f := newFixture(t, m, readyNode("node-a"))
	start := metav1.NewTime(f.clock.Now())
	m.Status.NotReadySince = &start
	if err := f.cl.Status().Update(context.Background(), m); err != nil {
		t.Fatalf("seed NotReadySince: %v", err)
	}

	f.reconcile(t)

	got := f.getMachine(t)
	if got.Status.State != v1alpha1.MachineStateReady {
		t.Errorf("state = %q, want Ready (node recovered)", got.Status.State)
	}
	if got.Status.NotReadySince != nil {
		t.Error("NotReadySince still set, want cleared on recovery")
	}
}

// normalPod returns a plain pod scheduled on the node, evictable by a drain.
func normalPod(name, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// nodePool returns a pool selecting Machines by the given labels, with the given
// drain timeout (nil leaves it at the controller default).
func nodePool(name string, matchLabels map[string]string, timeoutSeconds *int32) *v1alpha1.NodePool {
	return &v1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.NodePoolSpec{
			MachineSelector: metav1.LabelSelector{MatchLabels: matchLabels},
			Drain:           v1alpha1.DrainSpec{TimeoutSeconds: timeoutSeconds},
		},
	}
}

func TestReconcileReadyWithDrainAnnotationStartsDraining(t *testing.T) {
	t.Parallel()

	f := newFixture(t, machine(v1alpha1.MachineStateReady, map[string]string{
		v1alpha1.AnnotationDrainNow: v1alpha1.AnnotationDrainNowValue,
	}), readyNode("node-a"))

	res := f.reconcile(t)

	if !res.Requeue {
		t.Error("Requeue = false, want true after starting drain")
	}
	m := f.getMachine(t)
	if m.Status.State != v1alpha1.MachineStateDraining {
		t.Errorf("state = %q, want %q", m.Status.State, v1alpha1.MachineStateDraining)
	}
	if m.Status.DrainStartTime == nil {
		t.Error("DrainStartTime = nil, want stamped")
	}
	if _, ok := m.Annotations[v1alpha1.AnnotationDrainNow]; ok {
		t.Error("drain-now annotation still present, want removed (one-shot)")
	}
}

// TestReconcileReadyRefusesDrainOnAlwaysOnNode: drain-now on a Machine whose Node
// is labeled always-on is refused — the Machine stays Ready, the trigger is
// dropped, the Node is not cordoned, and a DrainRefused Event explains why.
func TestReconcileReadyRefusesDrainOnAlwaysOnNode(t *testing.T) {
	t.Parallel()

	node := readyNode("node-a")
	node.Labels = map[string]string{v1alpha1.LabelAlwaysOn: v1alpha1.LabelAlwaysOnValue}
	f := newFixture(t, machine(v1alpha1.MachineStateReady, map[string]string{
		v1alpha1.AnnotationDrainNow: v1alpha1.AnnotationDrainNowValue,
	}), node)

	f.reconcile(t)

	m := f.getMachine(t)
	if m.Status.State != v1alpha1.MachineStateReady {
		t.Errorf("state = %q, want %q", m.Status.State, v1alpha1.MachineStateReady)
	}
	if m.Status.DrainStartTime != nil {
		t.Error("DrainStartTime stamped, want nil (drain refused)")
	}
	if _, ok := m.Annotations[v1alpha1.AnnotationDrainNow]; ok {
		t.Error("drain-now annotation still present, want removed so the refusal does not loop")
	}
	if f.getNode(t, "node-a").Spec.Unschedulable {
		t.Error("always-on Node cordoned, want untouched")
	}
	assertEvent(t, f.r.Recorder.(*record.FakeRecorder), reasonDrainRefused)
}

func TestReconcileDrainingEvictsAndMovesToShuttingDown(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	start := metav1.NewTime(time.Now())
	m.Status.DrainStartTime = &start

	f := newFixture(t, m, readyNode("node-a"), normalPod("app-1", "node-a"))

	// First pass: the node is cordoned and nothing else happens — the scheduler
	// gets one poll to see the cordon before any emptiness judgement.
	res := f.reconcile(t)
	if res.RequeueAfter != drainPollInterval {
		t.Errorf("RequeueAfter = %v, want %v (settle after cordon)", res.RequeueAfter, drainPollInterval)
	}
	if len(f.evicted) != 0 {
		t.Errorf("evicted = %v, want none on the cordon pass", f.evicted)
	}
	if !f.getNode(t, "node-a").Spec.Unschedulable {
		t.Error("node not cordoned, want unschedulable=true")
	}

	// Second pass: the pod is evicted and the Machine stays Draining.
	res = f.reconcile(t)
	if res.RequeueAfter != drainPollInterval {
		t.Errorf("RequeueAfter = %v, want %v (eviction in flight)", res.RequeueAfter, drainPollInterval)
	}
	if len(f.evicted) != 1 || f.evicted[0] != "app-1" {
		t.Errorf("evicted = %v, want [app-1]", f.evicted)
	}
	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateDraining {
		t.Errorf("state = %q, want %q (still draining)", got.Status.State, v1alpha1.MachineStateDraining)
	}

	// Stub Evict deletes nothing, so simulate the pod going away, then reconcile
	// again: an empty node moves the Machine to ShuttingDown.
	if err := f.cl.Delete(context.Background(), normalPod("app-1", "node-a")); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	f.reconcile(t)

	got := f.getMachine(t)
	if got.Status.State != v1alpha1.MachineStateShuttingDown {
		t.Errorf("state = %q, want %q", got.Status.State, v1alpha1.MachineStateShuttingDown)
	}
	cond := condition(got, v1alpha1.ConditionDrainSucceeded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("DrainSucceeded condition = %+v, want status True", cond)
	}
}

func TestReconcileDrainingExcludesUnevictablePods(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	start := metav1.NewTime(time.Now())
	m.Status.DrainStartTime = &start

	dsPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ds-1", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds"}},
		},
		Spec:   corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	mirrorPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mirror-1", Namespace: "default",
			Annotations: map[string]string{mirrorPodAnnotation: "abc"},
		},
		Spec:   corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	f := newFixture(t, m, onpCordonedReadyNode("node-a"), dsPod, mirrorPod)

	f.reconcile(t)

	// Neither is evictable, so the node reads as drained and the Machine advances
	// with no Evict calls.
	if len(f.evicted) != 0 {
		t.Errorf("evicted = %v, want none (all pods unevictable)", f.evicted)
	}
	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateShuttingDown {
		t.Errorf("state = %q, want %q", got.Status.State, v1alpha1.MachineStateShuttingDown)
	}
}

// TestReconcileDrainingWaitsForTerminatingPod: an evicted pod still terminating
// keeps the node non-empty — the drain neither evicts it again nor moves to
// ShuttingDown until the pod is gone, so power-off never cuts its graceful
// shutdown short.
func TestReconcileDrainingWaitsForTerminatingPod(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	start := metav1.NewTime(time.Now())
	m.Status.DrainStartTime = &start
	deleting := normalPod("terminating-1", "node-a")
	delTime := metav1.NewTime(time.Now())
	deleting.DeletionTimestamp = &delTime
	deleting.Finalizers = []string{"keep-alive"} // a DeletionTimestamp needs a finalizer to persist

	f := newFixture(t, m, readyNode("node-a"), deleting)

	f.reconcile(t)

	if len(f.evicted) != 0 {
		t.Errorf("evicted = %v, want none (pod already terminating)", f.evicted)
	}
	if got := f.getMachine(t).Status.State; got != v1alpha1.MachineStateDraining {
		t.Fatalf("state = %q, want %q while the pod is still terminating", got, v1alpha1.MachineStateDraining)
	}

	// The pod finishes its shutdown: dropping the finalizer lets it go.
	var pod corev1.Pod
	if err := f.cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "terminating-1"}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	pod.Finalizers = nil
	if err := f.cl.Update(context.Background(), &pod); err != nil {
		t.Fatalf("release pod: %v", err)
	}
	f.reconcile(t)

	if got := f.getMachine(t).Status.State; got != v1alpha1.MachineStateShuttingDown {
		t.Errorf("state = %q, want %q once the pod is gone", got, v1alpha1.MachineStateShuttingDown)
	}
}

// TestReconcileDrainingAbortsOnAlwaysOnNode: an always-on label that appears
// while a drain runs stops it — no eviction, the ONP cordon lifted, the Machine
// back to Ready — so the node is never handed to the power-off leg.
func TestReconcileDrainingAbortsOnAlwaysOnNode(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	start := metav1.NewTime(time.Now())
	m.Status.DrainStartTime = &start
	node := onpCordonedReadyNode("node-a")
	node.Labels = map[string]string{v1alpha1.LabelAlwaysOn: v1alpha1.LabelAlwaysOnValue}
	f := newFixture(t, m, node, normalPod("app-1", "node-a"))

	f.reconcile(t)

	if len(f.evicted) != 0 {
		t.Errorf("evicted = %v, want none on an always-on node", f.evicted)
	}
	got := f.getMachine(t)
	if got.Status.State != v1alpha1.MachineStateReady {
		t.Errorf("state = %q, want %q", got.Status.State, v1alpha1.MachineStateReady)
	}
	if got.Status.DrainStartTime != nil {
		t.Error("DrainStartTime kept, want cleared")
	}
	if f.getNode(t, "node-a").Spec.Unschedulable {
		t.Error("node still cordoned, want the ONP cordon lifted")
	}
	assertEvent(t, f.r.Recorder.(*record.FakeRecorder), reasonDrainRefused)
}

// TestReconcileDrainingConfirmsEmptyWithAPI: the cache says the node is empty
// but the API server already holds a pod bound there — the drain keeps waiting
// instead of handing a node with workload to the power-off leg.
func TestReconcileDrainingConfirmsEmptyWithAPI(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	start := metav1.NewTime(time.Now())
	m.Status.DrainStartTime = &start
	f := newFixture(t, m, onpCordonedReadyNode("node-a"))
	// A second fake client stands in for the API server, where a pod just bound
	// to the node is already visible.
	f.r.APIReader = fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithIndex(&corev1.Pod{}, IndexPodNodeName, func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		WithObjects(normalPod("just-bound", "node-a")).
		Build()

	res := f.reconcile(t)

	if got := f.getMachine(t).Status.State; got != v1alpha1.MachineStateDraining {
		t.Errorf("state = %q, want %q while the API still shows a pod", got, v1alpha1.MachineStateDraining)
	}
	if res.RequeueAfter != drainPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, drainPollInterval)
	}
}

func TestReconcileDrainingTimesOutUncordonsAndFails(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	m.Labels = map[string]string{"pool": "a"} // pool selector matches on labels, not annotations
	f := newFixture(t, m, readyNode("node-a"),
		nodePool("pool-a", map[string]string{"pool": "a"}, ptrInt32(60)),
		normalPod("stuck-1", "node-a"))
	// Stamp DrainStartTime at the fake clock, cordon already applied, then step
	// past the pool's 60s timeout.
	start := metav1.NewTime(f.clock.Now())
	m.Status.DrainStartTime = &start
	if err := f.cl.Status().Update(context.Background(), m); err != nil {
		t.Fatalf("seed DrainStartTime: %v", err)
	}
	if _, err := f.r.setCordon(context.Background(), "node-a", true); err != nil {
		t.Fatalf("pre-cordon node: %v", err)
	}
	f.clock.Step(61 * time.Second)

	f.reconcile(t)

	if len(f.evicted) != 0 {
		t.Errorf("evicted = %v, want none (timeout stops before evicting)", f.evicted)
	}
	got := f.getMachine(t)
	if got.Status.State != v1alpha1.MachineStateFailed {
		t.Errorf("state = %q, want %q", got.Status.State, v1alpha1.MachineStateFailed)
	}
	cond := condition(got, v1alpha1.ConditionDrainSucceeded)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "DrainTimeout" {
		t.Errorf("DrainSucceeded condition = %+v, want False/DrainTimeout", cond)
	}
	var node corev1.Node
	if err := f.cl.Get(context.Background(), types.NamespacedName{Name: "node-a"}, &node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if node.Spec.Unschedulable {
		t.Error("node still cordoned, want uncordoned on the Failed path")
	}
}

func TestReconcileDrainingBlockedEvictionStaysDraining(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	start := metav1.NewTime(time.Now())
	m.Status.DrainStartTime = &start

	f := newFixture(t, m, onpCordonedReadyNode("node-a"), normalPod("pdb-1", "node-a"))
	// A PDB-blocked eviction comes back as TooManyRequests; the drain must treat
	// it as expected, not as a failure.
	f.evictErr = apierrors.NewTooManyRequests("disruption budget", 0)

	res := f.reconcile(t)

	if res.RequeueAfter != drainPollInterval {
		t.Errorf("RequeueAfter = %v, want %v (retry after poll)", res.RequeueAfter, drainPollInterval)
	}
	if len(f.evicted) != 1 {
		t.Errorf("evicted = %v, want one attempt", f.evicted)
	}
	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateDraining {
		t.Errorf("state = %q, want %q (blocked eviction does not fail the drain)", got.Status.State, v1alpha1.MachineStateDraining)
	}
}

// doNotDisruptPod returns a plain workload pod carrying the do-not-disrupt
// annotation.
func doNotDisruptPod(name, nodeName string) *corev1.Pod {
	p := normalPod(name, nodeName)
	p.Annotations = map[string]string{v1alpha1.AnnotationDoNotDisrupt: v1alpha1.AnnotationDoNotDisruptValue}
	return p
}

// TestReconcileDrainingSkipsDoNotDisruptPod: an unforced drain must not evict a
// do-not-disrupt pod, and the pod keeps the node non-empty, so the Machine stays
// Draining (it later times out into Failed) rather than powering off the node.
func TestReconcileDrainingSkipsDoNotDisruptPod(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	start := metav1.NewTime(time.Now())
	m.Status.DrainStartTime = &start

	f := newFixture(t, m, readyNode("node-a"), doNotDisruptPod("protected-1", "node-a"))

	res := f.reconcile(t)

	if res.RequeueAfter != drainPollInterval {
		t.Errorf("RequeueAfter = %v, want %v (protected pod blocks completion)", res.RequeueAfter, drainPollInterval)
	}
	if len(f.evicted) != 0 {
		t.Errorf("evicted = %v, want none (do-not-disrupt pod must not be evicted)", f.evicted)
	}
	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateDraining {
		t.Errorf("state = %q, want Draining (node not empty while protected pod runs)", got.Status.State)
	}
}

// TestReconcileDrainingForceEvictsDoNotDisruptPod: drain.force=true lifts the
// do-not-disrupt exemption — the pod is evicted like any other workload.
func TestReconcileDrainingForceEvictsDoNotDisruptPod(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	m.Labels = map[string]string{"pool": "a"}
	start := metav1.NewTime(time.Now())
	m.Status.DrainStartTime = &start

	pool := nodePool("pool-a", map[string]string{"pool": "a"}, nil)
	pool.Spec.Drain.Force = true

	f := newFixture(t, m, onpCordonedReadyNode("node-a"), pool, doNotDisruptPod("protected-1", "node-a"))

	f.reconcile(t)

	if len(f.evicted) != 1 || f.evicted[0] != "protected-1" {
		t.Errorf("evicted = %v, want [protected-1] (force evicts do-not-disrupt pods)", f.evicted)
	}
}

func TestDrainTimeoutResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		labels map[string]string
		pools  []client.Object
		want   time.Duration
	}{
		{
			name:   "matching pool with timeout uses pool value",
			labels: map[string]string{"pool": "a"},
			pools:  []client.Object{nodePool("pool-a", map[string]string{"pool": "a"}, ptrInt32(60))},
			want:   60 * time.Second,
		},
		{
			name:   "no matching pool uses default",
			labels: map[string]string{"pool": "other"},
			pools:  []client.Object{nodePool("pool-a", map[string]string{"pool": "a"}, ptrInt32(60))},
			want:   defaultDrainTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := machine(v1alpha1.MachineStateDraining, nil)
			m.Labels = tt.labels
			f := newFixture(t, append([]client.Object{m}, tt.pools...)...)

			got, _, err := f.r.drainPolicy(context.Background(), m)
			if err != nil {
				t.Fatalf("drainPolicy() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("drainPolicy() timeout = %v, want %v", got, tt.want)
			}
		})
	}
}

func ptrInt32(v int32) *int32 { return &v }

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

// events drains the fixture recorder's buffered Events channel without blocking.
func (f *reconcilerFixture) events(t *testing.T) []string {
	t.Helper()
	rec, ok := f.r.Recorder.(*record.FakeRecorder)
	if !ok {
		t.Fatalf("recorder is %T, want *record.FakeRecorder", f.r.Recorder)
	}
	var got []string
	for {
		select {
		case e := <-rec.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

// TestReconcileDrainingDoesNotClaimOperatorCordon: when ONP drains a node the
// operator had already cordoned (unschedulable, no onp marker), ONP must not stamp
// the marker — otherwise a later wake would auto-uncordon a cordon ONP never placed.
func TestReconcileDrainingDoesNotClaimOperatorCordon(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	start := metav1.NewTime(time.Now())
	m.Status.DrainStartTime = &start

	node := readyNode("node-a")
	node.Spec.Unschedulable = true // operator-cordoned: no onp marker

	f := newFixture(t, m, node, normalPod("app-1", "node-a"))

	f.reconcile(t)

	n := f.getNode(t, "node-a")
	if !n.Spec.Unschedulable {
		t.Error("operator cordon was lifted, want left in place")
	}
	if _, ok := n.Annotations[v1alpha1.AnnotationCordonedByONP]; ok {
		t.Error("ONP stamped its marker on an operator's cordon, want left unclaimed")
	}
	// The drain still proceeds — the pod is evicted regardless of who cordoned.
	if len(f.evicted) != 1 || f.evicted[0] != "app-1" {
		t.Errorf("evicted = %v, want [app-1]", f.evicted)
	}
}

// TestReconcileDrainingTimeoutLeavesOperatorCordon: on the drain-timeout Failed
// path, ONP uncordons only its own cordon — an operator's pre-existing cordon
// stays in place.
func TestReconcileDrainingTimeoutLeavesOperatorCordon(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	m.Labels = map[string]string{"pool": "a"}
	node := readyNode("node-a")
	node.Spec.Unschedulable = true // operator-cordoned: no onp marker

	f := newFixture(t, m, node,
		nodePool("pool-a", map[string]string{"pool": "a"}, ptrInt32(60)),
		doNotDisruptPod("protected-1", "node-a"))
	start := metav1.NewTime(f.clock.Now())
	m.Status.DrainStartTime = &start
	if err := f.cl.Status().Update(context.Background(), m); err != nil {
		t.Fatalf("seed DrainStartTime: %v", err)
	}
	f.clock.Step(61 * time.Second)

	f.reconcile(t)

	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateFailed {
		t.Fatalf("state = %q, want Failed", got.Status.State)
	}
	if n := f.getNode(t, "node-a"); !n.Spec.Unschedulable {
		t.Error("operator cordon lifted on timeout, want preserved (only ONP's own cordon is lifted)")
	}
}

// TestReconcileOffTidiesStaleDrainNow: a drain-now left on an Off Machine (set by
// hand, or surviving a failed cleanup) is removed, so it cannot re-drain the node
// the moment it is next woken.
func TestReconcileOffTidiesStaleDrainNow(t *testing.T) {
	t.Parallel()

	f := newFixture(t, machine(v1alpha1.MachineStateOff, map[string]string{
		v1alpha1.AnnotationDrainNow: v1alpha1.AnnotationDrainNowValue,
	}))

	f.reconcile(t)

	m := f.getMachine(t)
	if _, ok := m.Annotations[v1alpha1.AnnotationDrainNow]; ok {
		t.Error("stale drain-now still present on Off Machine, want tidied")
	}
	if m.Status.State != v1alpha1.MachineStateOff {
		t.Errorf("state = %q, want Off (no wake requested)", m.Status.State)
	}
}

// TestReconcileDrainingTidiesLeftoverDrainNow: if startDraining's cleanup patch
// failed, a drain-now can survive into Draining; reconcileDraining clears it so it
// does not re-drain the node after a later wake.
func TestReconcileDrainingTidiesLeftoverDrainNow(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, map[string]string{
		v1alpha1.AnnotationDrainNow: v1alpha1.AnnotationDrainNowValue,
	})
	start := metav1.NewTime(time.Now())
	m.Status.DrainStartTime = &start

	f := newFixture(t, m, readyNode("node-a"))

	f.reconcile(t)

	if _, ok := f.getMachine(t).Annotations[v1alpha1.AnnotationDrainNow]; ok {
		t.Error("leftover drain-now still present in Draining, want tidied")
	}
}

// TestReconcileBootingAppliesNodeTemplate: on the Ready transition ONP merges the
// pool Template labels (overlaid by the Machine's own labels) and Template taints
// onto the real Node, so it carries what the fit simulation assumed.
func TestReconcileBootingAppliesNodeTemplate(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateBooting, map[string]string{
		v1alpha1.AnnotationWakeNow: v1alpha1.AnnotationWakeNowValue,
	})
	m.Labels = map[string]string{"pool": "a"}        // selects the pool
	m.Spec.Labels = map[string]string{"disk": "ssd"} // node-specific, wins on conflict
	start := metav1.Now()
	m.Status.BootStartTime = &start

	pool := nodePool("pool-a", map[string]string{"pool": "a"}, nil)
	pool.Spec.Template.Labels = map[string]string{"onp.io/pool": "a", "disk": "hdd"}
	pool.Spec.Template.Taints = []corev1.Taint{{Key: "dedicated", Value: "a", Effect: corev1.TaintEffectNoSchedule}}

	f := newFixture(t, m, readyNode("node-a"), pool)

	f.reconcile(t)

	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateReady {
		t.Fatalf("state = %q, want Ready", got.Status.State)
	}
	n := f.getNode(t, "node-a")
	if n.Labels["onp.io/pool"] != "a" {
		t.Errorf("node label onp.io/pool = %q, want a (from pool template)", n.Labels["onp.io/pool"])
	}
	if n.Labels["disk"] != "ssd" {
		t.Errorf("node label disk = %q, want ssd (machine label overrides template)", n.Labels["disk"])
	}
	if !containsTaint(n.Spec.Taints, corev1.Taint{Key: "dedicated", Value: "a", Effect: corev1.TaintEffectNoSchedule}) {
		t.Errorf("node taints = %v, want template taint applied", n.Spec.Taints)
	}
}

// TestReconcileBootingWarnsOnCapacityDrift: when the now-Ready Node reports a
// capacity that differs from spec.capacity, ONP becomes Ready and emits a
// CapacityDrift warning (it does not auto-correct).
func TestReconcileBootingWarnsOnCapacityDrift(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateBooting, map[string]string{
		v1alpha1.AnnotationWakeNow: v1alpha1.AnnotationWakeNowValue,
	})
	m.Spec.Capacity = corev1.ResourceList{corev1.ResourceCPU: resourceQty("8")}
	start := metav1.Now()
	m.Status.BootStartTime = &start

	node := readyNode("node-a")
	node.Status.Capacity = corev1.ResourceList{corev1.ResourceCPU: resourceQty("4")}

	f := newFixture(t, m, node)

	f.reconcile(t)

	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateReady {
		t.Fatalf("state = %q, want Ready", got.Status.State)
	}
	if !hasEvent(f.events(t), reasonCapacityDrift) {
		t.Errorf("no %s event emitted, want one for the cpu mismatch", reasonCapacityDrift)
	}
}

// TestReconcileBootingNoCapacityDriftWhenMatching: matching capacity emits no
// drift warning.
func TestReconcileBootingNoCapacityDriftWhenMatching(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateBooting, map[string]string{
		v1alpha1.AnnotationWakeNow: v1alpha1.AnnotationWakeNowValue,
	})
	m.Spec.Capacity = corev1.ResourceList{corev1.ResourceCPU: resourceQty("4")}
	start := metav1.Now()
	m.Status.BootStartTime = &start

	node := readyNode("node-a")
	node.Status.Capacity = corev1.ResourceList{corev1.ResourceCPU: resourceQty("4")}

	f := newFixture(t, m, node)

	f.reconcile(t)

	if hasEvent(f.events(t), reasonCapacityDrift) {
		t.Error("CapacityDrift event emitted, want none (capacity matches)")
	}
}

// hasEvent reports whether any event string contains reason.
func hasEvent(events []string, reason string) bool {
	for _, e := range events {
		if strings.Contains(e, reason) {
			return true
		}
	}
	return false
}

// resourceQty parses a resource.Quantity for tests, failing the build only if the
// literal is malformed (it never is at call sites).
func resourceQty(s string) resource.Quantity {
	return resource.MustParse(s)
}

// containsTaint reports whether taints has an entry equal to t.
func containsTaint(taints []corev1.Taint, t corev1.Taint) bool {
	for _, have := range taints {
		if have.Key == t.Key && have.Value == t.Value && have.Effect == t.Effect {
			return true
		}
	}
	return false
}

func TestMergeTaint(t *testing.T) {
	t.Parallel()

	ns := corev1.Taint{Key: "onp.io/on-demand", Value: "true", Effect: corev1.TaintEffectPreferNoSchedule}
	tests := []struct {
		name        string
		have        []corev1.Taint
		add         corev1.Taint
		want        []corev1.Taint
		wantChanged bool
	}{
		{"absent is appended", nil, ns, []corev1.Taint{ns}, true},
		{"identical is a no-op", []corev1.Taint{ns}, ns, []corev1.Taint{ns}, false},
		{
			"same key and effect replaces the value",
			[]corev1.Taint{ns},
			corev1.Taint{Key: ns.Key, Value: "yes", Effect: ns.Effect},
			[]corev1.Taint{{Key: ns.Key, Value: "yes", Effect: ns.Effect}},
			true,
		},
		{
			"same key, other effect is a separate taint",
			[]corev1.Taint{ns},
			corev1.Taint{Key: ns.Key, Value: "true", Effect: corev1.TaintEffectNoSchedule},
			[]corev1.Taint{ns, {Key: ns.Key, Value: "true", Effect: corev1.TaintEffectNoSchedule}},
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := mergeTaint(append([]corev1.Taint(nil), tc.have...), tc.add)
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("taints = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("taints[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestReconcileBootingFailsOnRejectedTemplate: when the API server rejects the
// template patch as invalid — a configuration error no retry fixes — the Machine
// fails with TemplateRejected instead of staying Booting forever, where it would
// hold back every other wake for the same pod.
func TestReconcileBootingFailsOnRejectedTemplate(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateBooting, nil)
	start := metav1.Now()
	m.Status.BootStartTime = &start
	m.Spec.Labels = map[string]string{"gpu-model": "RTX 3090"}
	f := newFixture(t, m, readyNode("node-a"))
	f.r.Client = interceptor.NewClient(f.cl.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isNode := obj.(*corev1.Node); isNode {
				return apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Node").GroupKind(), obj.GetName(), nil)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	f.reconcile(t)

	got := f.getMachine(t)
	if got.Status.State != v1alpha1.MachineStateFailed {
		t.Fatalf("state = %q, want %q", got.Status.State, v1alpha1.MachineStateFailed)
	}
	if cond := condition(got, v1alpha1.ConditionReady); cond == nil || cond.Reason != reasonTemplateRejected {
		t.Errorf("Ready condition = %+v, want reason %s", cond, reasonTemplateRejected)
	}
}

// TestDrainPolicyConservativeOnPoolConflict: a Machine matching two pools drains
// under the most conservative reading of both — never force, the shortest
// budget — whatever order the pools are listed in.
func TestDrainPolicyConservativeOnPoolConflict(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	m.Labels = map[string]string{"pool": "a", "dup": "yes"}
	forcing := nodePool("pool-a", map[string]string{"pool": "a"}, ptrInt32(600))
	forcing.Spec.Drain.Force = true
	strict := nodePool("pool-b", map[string]string{"dup": "yes"}, ptrInt32(60))
	f := newFixture(t, m, forcing, strict)

	timeout, force, err := f.r.drainPolicy(context.Background(), m)
	if err != nil {
		t.Fatalf("drainPolicy() error: %v", err)
	}
	if force {
		t.Error("force = true, want false when the pools disagree")
	}
	if timeout != 60*time.Second {
		t.Errorf("timeout = %v, want the shortest budget 60s", timeout)
	}
}

// TestReconcileBootingSkipsPoolTemplateOnConflict: a Machine matching two pools
// gets neither pool's template on the Node — only its own labels — and a
// PoolConflict Event, instead of whichever template the cache listed first.
func TestReconcileBootingSkipsPoolTemplateOnConflict(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateBooting, nil)
	start := metav1.Now()
	m.Status.BootStartTime = &start
	m.Labels = map[string]string{"pool": "a", "dup": "yes"}
	m.Spec.Labels = map[string]string{"team": "a"}
	poolA := nodePool("pool-a", map[string]string{"pool": "a"}, nil)
	poolA.Spec.Template.Labels = map[string]string{"from": "a"}
	poolB := nodePool("pool-b", map[string]string{"dup": "yes"}, nil)
	poolB.Spec.Template.Labels = map[string]string{"from": "b"}
	f := newFixture(t, m, poolA, poolB, readyNode("node-a"))

	f.reconcile(t)

	labels := f.getNode(t, "node-a").Labels
	if _, ok := labels["from"]; ok {
		t.Errorf("pool template label applied (from=%q), want none on conflict", labels["from"])
	}
	if labels["team"] != "a" {
		t.Errorf("team label = %q, want the Machine's own label applied", labels["team"])
	}
	assertEvent(t, f.r.Recorder.(*record.FakeRecorder), reasonPoolConflict)
}
