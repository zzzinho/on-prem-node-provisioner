package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

// scaleDownBase is the fixed instant the fake clock starts at, so consolidateAfter
// math in tests reads deterministically.
var scaleDownBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// sdLabels places a Machine in the test pool.
var sdLabels = map[string]string{"pool": "edge"}

// sdMachine builds a pool-member Machine in the given state. emptySince, when
// non-nil, seeds status.emptySince to model a node already mid-timer.
func sdMachine(name string, state v1alpha1.MachineState, emptySince *time.Time) *v1alpha1.Machine {
	m := &v1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: sdLabels},
		Spec: v1alpha1.MachineSpec{
			NodeName: name,
			Power:    v1alpha1.PowerSpec{Provider: "wol", WoL: &v1alpha1.WoLConfig{MacAddress: "aa:bb:cc:dd:ee:ff"}},
		},
	}
	m.Status.State = state
	if emptySince != nil {
		t := metav1.NewTime(*emptySince)
		m.Status.EmptySince = &t
	}
	return m
}

// whenEmptyPool builds a pool selecting sdLabels with the WhenEmpty policy. A nil
// after leaves consolidateAfter unset (scale-down disabled).
func whenEmptyPool(name string, after *time.Duration) *v1alpha1.NodePool {
	p := &v1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.NodePoolSpec{
			MachineSelector: metav1.LabelSelector{MatchLabels: sdLabels},
			Disruption:      v1alpha1.DisruptionSpec{ConsolidationPolicy: v1alpha1.ConsolidationPolicyWhenEmpty},
		},
	}
	if after != nil {
		p.Spec.Disruption.ConsolidateAfter = &metav1.Duration{Duration: *after}
	}
	return p
}

// sdPod builds a Running pod scheduled on node. kind, when set to "DaemonSet",
// makes it owner-excluded; "mirror" marks it a static pod. "" is a plain
// evictable workload pod.
func sdPod(name, node, kind string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	switch kind {
	case "DaemonSet":
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "ds", UID: "u"}}
	case "mirror":
		p.Annotations = map[string]string{mirrorPodAnnotation: "node/x"}
	}
	return p
}

func newScaleDownReconciler(t *testing.T, rec record.EventRecorder, clk *clocktesting.FakePassiveClock, objs ...client.Object) (*ScaleDownReconciler, client.Client) {
	t.Helper()
	scheme := newScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Machine{}, &v1alpha1.NodePool{}).
		WithIndex(&corev1.Pod{}, IndexPodNodeName, func(o client.Object) []string {
			pod, ok := o.(*corev1.Pod)
			if !ok || pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}).
		WithIndex(&v1alpha1.Machine{}, IndexMachineNodeName, func(o client.Object) []string {
			m, ok := o.(*v1alpha1.Machine)
			if !ok || m.Spec.NodeName == "" {
				return nil
			}
			return []string{m.Spec.NodeName}
		}).
		WithObjects(objs...).
		Build()
	return &ScaleDownReconciler{Client: cl, Scheme: scheme, Recorder: rec, Clock: clk}, cl
}

// reconcileSD reconciles the named Machine once.
func reconcileSD(t *testing.T, r *ScaleDownReconciler, name string) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("reconcile %q: %v", name, err)
	}
	return res
}

// getSDMachine re-reads a Machine from the client.
func getSDMachine(t *testing.T, cl client.Client, name string) *v1alpha1.Machine {
	t.Helper()
	var m v1alpha1.Machine
	if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, &m); err != nil {
		t.Fatalf("get machine %q: %v", name, err)
	}
	return &m
}

func drainNowSet(m *v1alpha1.Machine) bool {
	return m.Annotations[v1alpha1.AnnotationDrainNow] == v1alpha1.AnnotationDrainNowValue
}

// assertEvent fails unless an event whose message contains reason was recorded.
func assertEvent(t *testing.T, rec *record.FakeRecorder, reason string) {
	t.Helper()
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, reason) {
				return
			}
		default:
			t.Fatalf("no event containing %q recorded", reason)
		}
	}
}

// TestScaleDownStampsEmptySinceOnFirstEmptyObservation: a Ready, empty Machine in
// a WhenEmpty pool gets its timer anchored and is requeued for the full window —
// not drained yet.
func TestScaleDownStampsEmptySinceOnFirstEmptyObservation(t *testing.T) {
	after := 5 * time.Minute
	m := sdMachine("node-a", v1alpha1.MachineStateReady, nil)
	pool := whenEmptyPool("edge", &after)
	clk := clocktesting.NewFakePassiveClock(scaleDownBase)
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, pool)

	res := reconcileSD(t, r, "node-a")

	if res.RequeueAfter != after {
		t.Fatalf("RequeueAfter = %s, want %s", res.RequeueAfter, after)
	}
	got := getSDMachine(t, cl, "node-a")
	if got.Status.EmptySince == nil {
		t.Fatal("emptySince not stamped")
	}
	if !got.Status.EmptySince.Time.Equal(scaleDownBase) {
		t.Fatalf("emptySince = %s, want %s", got.Status.EmptySince.Time, scaleDownBase)
	}
	if drainNowSet(got) {
		t.Fatal("drain-now set on first empty observation; should only fire after consolidateAfter")
	}
}

// TestScaleDownWaitsWhileWindowOpen: anchored but not yet elapsed -> requeue for
// the remaining time, still no drain.
func TestScaleDownWaitsWhileWindowOpen(t *testing.T) {
	after := 5 * time.Minute
	empty := scaleDownBase
	m := sdMachine("node-a", v1alpha1.MachineStateReady, &empty)
	pool := whenEmptyPool("edge", &after)
	clk := clocktesting.NewFakePassiveClock(scaleDownBase.Add(2 * time.Minute)) // 3m remaining
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, pool)

	res := reconcileSD(t, r, "node-a")

	if want := 3 * time.Minute; res.RequeueAfter != want {
		t.Fatalf("RequeueAfter = %s, want %s", res.RequeueAfter, want)
	}
	if drainNowSet(getSDMachine(t, cl, "node-a")) {
		t.Fatal("drain-now set before consolidateAfter elapsed")
	}
}

// TestScaleDownTriggersDrainAfterWindow: empty for the whole window -> drain-now
// is set and a ScaleDown event is emitted.
func TestScaleDownTriggersDrainAfterWindow(t *testing.T) {
	after := 5 * time.Minute
	empty := scaleDownBase
	m := sdMachine("node-a", v1alpha1.MachineStateReady, &empty)
	pool := whenEmptyPool("edge", &after)
	clk := clocktesting.NewFakePassiveClock(scaleDownBase.Add(after)) // exactly elapsed
	rec := record.NewFakeRecorder(8)
	r, cl := newScaleDownReconciler(t, rec, clk, m, pool)

	res := reconcileSD(t, r, "node-a")

	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %s, want 0 after trigger", res.RequeueAfter)
	}
	if !drainNowSet(getSDMachine(t, cl, "node-a")) {
		t.Fatal("drain-now not set after consolidateAfter elapsed")
	}
	assertEvent(t, rec, reasonScaleDown)
}

// TestScaleDownClearsTimerWhenNodeNotEmpty: a workload pod on the node resets the
// timer — emptySince is cleared and no drain fires.
func TestScaleDownClearsTimerWhenNodeNotEmpty(t *testing.T) {
	after := 5 * time.Minute
	empty := scaleDownBase
	m := sdMachine("node-a", v1alpha1.MachineStateReady, &empty)
	pool := whenEmptyPool("edge", &after)
	pod := sdPod("app", "node-a", "")
	clk := clocktesting.NewFakePassiveClock(scaleDownBase.Add(10 * time.Minute)) // past window
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, pool, pod)

	reconcileSD(t, r, "node-a")

	got := getSDMachine(t, cl, "node-a")
	if got.Status.EmptySince != nil {
		t.Fatalf("emptySince = %v, want nil (node not empty)", got.Status.EmptySince)
	}
	if drainNowSet(got) {
		t.Fatal("drain-now set while node carries workload")
	}
}

// TestScaleDownTreatsDaemonSetAndMirrorPodsAsEmpty: only unevictable pods on the
// node still counts as empty, so the timer starts.
func TestScaleDownTreatsDaemonSetAndMirrorPodsAsEmpty(t *testing.T) {
	after := 5 * time.Minute
	m := sdMachine("node-a", v1alpha1.MachineStateReady, nil)
	pool := whenEmptyPool("edge", &after)
	ds := sdPod("kube-proxy", "node-a", "DaemonSet")
	mirror := sdPod("static", "node-a", "mirror")
	clk := clocktesting.NewFakePassiveClock(scaleDownBase)
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, pool, ds, mirror)

	reconcileSD(t, r, "node-a")

	if getSDMachine(t, cl, "node-a").Status.EmptySince == nil {
		t.Fatal("emptySince not stamped; DaemonSet/mirror pods should not count as workload")
	}
}

// TestScaleDownDisabledWithoutConsolidateAfter: WhenEmpty policy but no
// consolidateAfter -> scale-down is off, a stale timer is cleared, nothing drains.
func TestScaleDownDisabledWithoutConsolidateAfter(t *testing.T) {
	empty := scaleDownBase
	m := sdMachine("node-a", v1alpha1.MachineStateReady, &empty)
	pool := whenEmptyPool("edge", nil) // consolidateAfter unset
	clk := clocktesting.NewFakePassiveClock(scaleDownBase.Add(time.Hour))
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, pool)

	reconcileSD(t, r, "node-a")

	got := getSDMachine(t, cl, "node-a")
	if got.Status.EmptySince != nil {
		t.Fatalf("emptySince = %v, want nil (scale-down disabled)", got.Status.EmptySince)
	}
	if drainNowSet(got) {
		t.Fatal("drain-now set with no consolidateAfter; scale-down must stay disabled")
	}
}

// TestScaleDownIgnoresNonReadyMachine: a non-Ready Machine is not a candidate; a
// stale timer is cleared and nothing drains.
func TestScaleDownIgnoresNonReadyMachine(t *testing.T) {
	after := 5 * time.Minute
	empty := scaleDownBase
	m := sdMachine("node-a", v1alpha1.MachineStateBooting, &empty)
	pool := whenEmptyPool("edge", &after)
	clk := clocktesting.NewFakePassiveClock(scaleDownBase.Add(time.Hour))
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, pool)

	reconcileSD(t, r, "node-a")

	got := getSDMachine(t, cl, "node-a")
	if got.Status.EmptySince != nil {
		t.Fatalf("emptySince = %v, want nil (machine not Ready)", got.Status.EmptySince)
	}
	if drainNowSet(got) {
		t.Fatal("drain-now set on a non-Ready machine")
	}
}

// TestScaleDownNoOpWhenDrainAlreadyRequested: an existing drain-now (operator or a
// prior reconcile) means the node is on its way down — do not re-stamp or re-fire.
func TestScaleDownNoOpWhenDrainAlreadyRequested(t *testing.T) {
	after := 5 * time.Minute
	m := sdMachine("node-a", v1alpha1.MachineStateReady, nil)
	m.Annotations = map[string]string{v1alpha1.AnnotationDrainNow: v1alpha1.AnnotationDrainNowValue}
	pool := whenEmptyPool("edge", &after)
	clk := clocktesting.NewFakePassiveClock(scaleDownBase)
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, pool)

	res := reconcileSD(t, r, "node-a")

	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %s, want 0 (no-op)", res.RequeueAfter)
	}
	if getSDMachine(t, cl, "node-a").Status.EmptySince != nil {
		t.Fatal("emptySince stamped while a drain was already requested")
	}
}

// TestScaleDownNoPoolDisabled: a Machine matching no pool has scale-down off.
func TestScaleDownNoPoolDisabled(t *testing.T) {
	m := sdMachine("node-a", v1alpha1.MachineStateReady, nil)
	m.Labels = map[string]string{"pool": "other"} // matches no pool
	after := 5 * time.Minute
	pool := whenEmptyPool("edge", &after)
	clk := clocktesting.NewFakePassiveClock(scaleDownBase)
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, pool)

	reconcileSD(t, r, "node-a")

	if getSDMachine(t, cl, "node-a").Status.EmptySince != nil {
		t.Fatal("emptySince stamped for a machine in no pool")
	}
}

// TestScaleDownRefusesBelowMinNodes: a single empty member in a pool with
// minNodes=1 is the floor — draining it would breach the floor, so it is held.
func TestScaleDownRefusesBelowMinNodes(t *testing.T) {
	after := 5 * time.Minute
	empty := scaleDownBase
	m := sdMachine("node-a", v1alpha1.MachineStateReady, &empty)
	pool := whenEmptyPool("edge", &after)
	pool.Spec.MinNodes = 1
	clk := clocktesting.NewFakePassiveClock(scaleDownBase.Add(after))
	rec := record.NewFakeRecorder(8)
	r, cl := newScaleDownReconciler(t, rec, clk, m, pool)

	res := reconcileSD(t, r, "node-a")

	if res.RequeueAfter != scaleDownBlockedRequeue {
		t.Fatalf("RequeueAfter = %s, want %s (blocked by minNodes)", res.RequeueAfter, scaleDownBlockedRequeue)
	}
	if drainNowSet(getSDMachine(t, cl, "node-a")) {
		t.Fatal("drain-now set despite minNodes floor")
	}
	assertEvent(t, rec, reasonScaleDownBlocked)
}

// TestScaleDownAllowedAboveMinNodes: with a second member keeping the pool above
// its floor, the empty node drains.
func TestScaleDownAllowedAboveMinNodes(t *testing.T) {
	after := 5 * time.Minute
	empty := scaleDownBase
	m := sdMachine("node-a", v1alpha1.MachineStateReady, &empty)
	keep := sdMachine("node-b", v1alpha1.MachineStateReady, nil)
	pool := whenEmptyPool("edge", &after)
	pool.Spec.MinNodes = 1
	clk := clocktesting.NewFakePassiveClock(scaleDownBase.Add(after))
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, keep, pool)

	reconcileSD(t, r, "node-a")

	if !drainNowSet(getSDMachine(t, cl, "node-a")) {
		t.Fatal("drain-now not set; pool was above its minNodes floor")
	}
}

// TestScaleDownDefersAtMaxConcurrent: a member already Draining occupies the
// single concurrency slot, so a second empty node defers rather than draining too.
func TestScaleDownDefersAtMaxConcurrent(t *testing.T) {
	after := 5 * time.Minute
	empty := scaleDownBase
	m := sdMachine("node-a", v1alpha1.MachineStateReady, &empty)
	draining := sdMachine("node-b", v1alpha1.MachineStateDraining, nil)
	pool := whenEmptyPool("edge", &after)
	one := int32(1)
	pool.Spec.Disruption.MaxConcurrent = &one
	clk := clocktesting.NewFakePassiveClock(scaleDownBase.Add(after))
	rec := record.NewFakeRecorder(8)
	r, cl := newScaleDownReconciler(t, rec, clk, m, draining, pool)

	res := reconcileSD(t, r, "node-a")

	if res.RequeueAfter != scaleDownBlockedRequeue {
		t.Fatalf("RequeueAfter = %s, want %s (maxConcurrent reached)", res.RequeueAfter, scaleDownBlockedRequeue)
	}
	if drainNowSet(getSDMachine(t, cl, "node-a")) {
		t.Fatal("drain-now set despite maxConcurrent cap")
	}
	assertEvent(t, rec, reasonScaleDownBlocked)
}

// TestScaleDownDefersDuringCooldown: a recent scale-down keeps the pool in its
// cooldown.scaleDown window; the next empty node waits exactly until it lifts.
func TestScaleDownDefersDuringCooldown(t *testing.T) {
	after := 5 * time.Minute
	empty := scaleDownBase
	m := sdMachine("node-a", v1alpha1.MachineStateReady, &empty)
	pool := whenEmptyPool("edge", &after)
	pool.Spec.Cooldown.ScaleDown = &metav1.Duration{Duration: 10 * time.Minute}
	last := metav1.NewTime(scaleDownBase.Add(after))
	pool.Status.LastScaleDownTime = &last
	// 4m past the last scale-down: 6m of the 10m cooldown remains.
	clk := clocktesting.NewFakePassiveClock(scaleDownBase.Add(after).Add(4 * time.Minute))
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, pool)

	res := reconcileSD(t, r, "node-a")

	if want := 6 * time.Minute; res.RequeueAfter != want {
		t.Fatalf("RequeueAfter = %s, want %s (scale-down cooldown)", res.RequeueAfter, want)
	}
	if drainNowSet(getSDMachine(t, cl, "node-a")) {
		t.Fatal("drain-now set during scale-down cooldown")
	}
}

// TestScaleDownStampsCooldownOnTrigger: the first scale-down (no prior cooldown)
// fires and records LastScaleDownTime so the next one is rate-limited.
func TestScaleDownStampsCooldownOnTrigger(t *testing.T) {
	after := 5 * time.Minute
	empty := scaleDownBase
	m := sdMachine("node-a", v1alpha1.MachineStateReady, &empty)
	pool := whenEmptyPool("edge", &after)
	pool.Spec.Cooldown.ScaleDown = &metav1.Duration{Duration: 10 * time.Minute}
	clk := clocktesting.NewFakePassiveClock(scaleDownBase.Add(after))
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, pool)

	reconcileSD(t, r, "node-a")

	if !drainNowSet(getSDMachine(t, cl, "node-a")) {
		t.Fatal("drain-now not set; no prior cooldown should block the first scale-down")
	}
	var got v1alpha1.NodePool
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "edge"}, &got); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if got.Status.LastScaleDownTime == nil {
		t.Fatal("LastScaleDownTime not stamped on scale-down trigger")
	}
}

// TestScaleDownSkipsDoNotDisruptNode: a Node marked do-not-disrupt is exempt from
// automatic scale-down — its empty timer never starts.
func TestScaleDownSkipsDoNotDisruptNode(t *testing.T) {
	after := 5 * time.Minute
	m := sdMachine("node-a", v1alpha1.MachineStateReady, nil)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "node-a",
		Annotations: map[string]string{v1alpha1.AnnotationDoNotDisrupt: v1alpha1.AnnotationDoNotDisruptValue},
	}}
	pool := whenEmptyPool("edge", &after)
	clk := clocktesting.NewFakePassiveClock(scaleDownBase)
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, node, pool)

	reconcileSD(t, r, "node-a")

	if getSDMachine(t, cl, "node-a").Status.EmptySince != nil {
		t.Fatal("emptySince stamped for a do-not-disrupt node; scale-down must skip it")
	}
}

// TestScaleDownTreatsDoNotDisruptPodAsWorkload: a do-not-disrupt pod keeps its node
// non-empty, so the node is never auto-targeted — no special case beyond counting
// the pod as workload.
func TestScaleDownTreatsDoNotDisruptPodAsWorkload(t *testing.T) {
	after := 5 * time.Minute
	m := sdMachine("node-a", v1alpha1.MachineStateReady, nil)
	pod := sdPod("protected", "node-a", "")
	pod.Annotations = map[string]string{v1alpha1.AnnotationDoNotDisrupt: v1alpha1.AnnotationDoNotDisruptValue}
	pool := whenEmptyPool("edge", &after)
	clk := clocktesting.NewFakePassiveClock(scaleDownBase)
	r, cl := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m, pod, pool)

	reconcileSD(t, r, "node-a")

	if getSDMachine(t, cl, "node-a").Status.EmptySince != nil {
		t.Fatal("emptySince stamped despite a do-not-disrupt workload pod; node is not empty")
	}
}

// TestScaleDownMachinesForPod maps a pod on a node to the Machine backing it.
func TestScaleDownMachinesForPod(t *testing.T) {
	m := sdMachine("node-a", v1alpha1.MachineStateReady, nil)
	clk := clocktesting.NewFakePassiveClock(scaleDownBase)
	r, _ := newScaleDownReconciler(t, record.NewFakeRecorder(8), clk, m)

	reqs := r.machinesForPod(context.Background(), sdPod("app", "node-a", ""))
	if len(reqs) != 1 || reqs[0].Name != "node-a" {
		t.Fatalf("machinesForPod = %v, want one request for node-a", reqs)
	}
	// An unscheduled pod (no NodeName) maps to nothing.
	if reqs := r.machinesForPod(context.Background(), sdPod("app", "", "")); len(reqs) != 0 {
		t.Fatalf("machinesForPod(unscheduled) = %v, want none", reqs)
	}
}
