package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func TestReconcileDrainingEvictsAndMovesToShuttingDown(t *testing.T) {
	t.Parallel()

	m := machine(v1alpha1.MachineStateDraining, nil)
	start := metav1.NewTime(time.Now())
	m.Status.DrainStartTime = &start

	f := newFixture(t, m, readyNode("node-a"), normalPod("app-1", "node-a"))

	// First pass: node has one evictable pod. It should be cordoned, the pod
	// evicted, and the Machine left Draining with a poll requeue.
	res := f.reconcile(t)
	if res.RequeueAfter != drainPollInterval {
		t.Errorf("RequeueAfter = %v, want %v (eviction in flight)", res.RequeueAfter, drainPollInterval)
	}
	if len(f.evicted) != 1 || f.evicted[0] != "app-1" {
		t.Errorf("evicted = %v, want [app-1]", f.evicted)
	}
	var node corev1.Node
	if err := f.cl.Get(context.Background(), types.NamespacedName{Name: "node-a"}, &node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if !node.Spec.Unschedulable {
		t.Error("node not cordoned, want unschedulable=true")
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
	deleting := normalPod("terminating-1", "node-a")
	delTime := metav1.NewTime(time.Now())
	deleting.DeletionTimestamp = &delTime
	deleting.Finalizers = []string{"keep-alive"} // a DeletionTimestamp needs a finalizer to persist

	f := newFixture(t, m, readyNode("node-a"), dsPod, mirrorPod, deleting)

	f.reconcile(t)

	// None of the three are evictable, so the node reads as drained and the
	// Machine advances with no Evict calls.
	if len(f.evicted) != 0 {
		t.Errorf("evicted = %v, want none (all pods unevictable)", f.evicted)
	}
	if got := f.getMachine(t); got.Status.State != v1alpha1.MachineStateShuttingDown {
		t.Errorf("state = %q, want %q", got.Status.State, v1alpha1.MachineStateShuttingDown)
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
	if err := f.r.setCordon(context.Background(), "node-a", true); err != nil {
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

	f := newFixture(t, m, readyNode("node-a"), normalPod("pdb-1", "node-a"))
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

			got, err := f.r.drainTimeout(context.Background(), m)
			if err != nil {
				t.Fatalf("drainTimeout() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("drainTimeout() = %v, want %v", got, tt.want)
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
