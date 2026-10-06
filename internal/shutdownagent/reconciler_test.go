package shutdownagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

const thisNode = "node-a"

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add v1alpha1 to scheme: %v", err)
	}
	return s
}

// machine builds a Machine for the given node in the given state.
func machine(name, nodeName string, state v1alpha1.MachineState) *v1alpha1.Machine {
	m := &v1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.MachineSpec{NodeName: nodeName},
	}
	m.Status.State = state
	return m
}

// fixture wires a ShutdownReconciler with a stub PowerOff that records calls.
type fixture struct {
	r        *ShutdownReconciler
	recorder *record.FakeRecorder
	calls    *int
}

func newFixture(t *testing.T, powerOffErr error, objs ...client.Object) *fixture {
	t.Helper()
	cl := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objs...).
		Build()
	rec := record.NewFakeRecorder(16)
	calls := 0
	return &fixture{
		r: &ShutdownReconciler{
			Client:   cl,
			NodeName: thisNode,
			PowerOff: func(context.Context) error {
				calls++
				return powerOffErr
			},
			Recorder: rec,
		},
		recorder: rec,
		calls:    &calls,
	}
}

func (f *fixture) reconcile(t *testing.T, name string) {
	t.Helper()
	if _, err := f.r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name},
	}); err != nil {
		t.Fatalf("Reconcile() unexpected error: %v", err)
	}
}

func TestReconcilePowersOff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		machine   *v1alpha1.Machine
		reqName   string // request name; defaults to machine name when empty
		wantCalls int
		wantEvent bool
	}{
		{
			name:      "own node ShuttingDown powers off",
			machine:   machine("node-a", thisNode, v1alpha1.MachineStateShuttingDown),
			wantCalls: 1,
			wantEvent: true,
		},
		{
			name:      "another node ShuttingDown is ignored",
			machine:   machine("node-b", "node-b", v1alpha1.MachineStateShuttingDown),
			wantCalls: 0,
		},
		{
			name:      "own node Ready does not power off",
			machine:   machine("node-a", thisNode, v1alpha1.MachineStateReady),
			wantCalls: 0,
		},
		{
			name:      "own node Off does not power off",
			machine:   machine("node-a", thisNode, v1alpha1.MachineStateOff),
			wantCalls: 0,
		},
		{
			name:      "missing machine is a no-op",
			machine:   nil,
			reqName:   "ghost",
			wantCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var objs []client.Object
			name := tt.reqName
			if tt.machine != nil {
				objs = append(objs, tt.machine)
				if name == "" {
					name = tt.machine.Name
				}
			}
			f := newFixture(t, nil, objs...)

			f.reconcile(t, name)

			if *f.calls != tt.wantCalls {
				t.Errorf("PowerOff calls = %d, want %d", *f.calls, tt.wantCalls)
			}
			if got := drainEvents(f.recorder); got != tt.wantEvent {
				t.Errorf("event emitted = %v, want %v", got, tt.wantEvent)
			}
		})
	}
}

// TestReconcileHonorsShutdownProvider: the agent powers off only when the Machine
// selects the agent shutdown provider (or none, the default). A non-agent provider
// — a future hard-cut path — must not also halt the host here.
func TestReconcileHonorsShutdownProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		provider  string // value for spec.shutdown.provider; "" leaves Shutdown nil
		wantCalls int
	}{
		{name: "nil shutdown spec powers off (default agent)", provider: "", wantCalls: 1},
		{name: "explicit agent provider powers off", provider: v1alpha1.ShutdownProviderAgent, wantCalls: 1},
		{name: "non-agent provider does not power off", provider: "ipmi", wantCalls: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := machine("node-a", thisNode, v1alpha1.MachineStateShuttingDown)
			if tt.provider != "" {
				m.Spec.Shutdown = &v1alpha1.ShutdownSpec{Provider: tt.provider}
			}
			f := newFixture(t, nil, m)

			f.reconcile(t, "node-a")

			if *f.calls != tt.wantCalls {
				t.Errorf("PowerOff calls = %d, want %d", *f.calls, tt.wantCalls)
			}
		})
	}
}

// TestReconcileIsIdempotent verifies the sync.Once guard: repeated reconciles of
// the same ShuttingDown Machine issue exactly one power-off.
func TestReconcileIsIdempotent(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil, machine("node-a", thisNode, v1alpha1.MachineStateShuttingDown))

	f.reconcile(t, "node-a")
	f.reconcile(t, "node-a")
	f.reconcile(t, "node-a")

	if *f.calls != 1 {
		t.Errorf("PowerOff calls = %d, want 1 (idempotent across reconciles)", *f.calls)
	}
}

// TestReconcilePowerOffErrorRetries verifies a failed power-off surfaces as a
// reconcile error and re-arms the guard so the next reconcile retries.
func TestReconcilePowerOffErrorRetries(t *testing.T) {
	t.Parallel()

	cl := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(machine("node-a", thisNode, v1alpha1.MachineStateShuttingDown)).
		Build()
	calls := 0
	powerOffErr := errors.New("nsenter failed")
	r := &ShutdownReconciler{
		Client:   cl,
		NodeName: thisNode,
		PowerOff: func(context.Context) error {
			calls++
			return powerOffErr
		},
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "node-a"}}
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("Reconcile() err = nil, want error on power-off failure")
	}

	// The failure re-armed the guard: a retry runs PowerOff again. Let it succeed.
	powerOffErr = nil
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile() retry unexpected error: %v", err)
	}
	if calls != 2 {
		t.Errorf("PowerOff calls = %d, want 2 (retry after transient failure)", calls)
	}
}

// drainEvents reports whether the recorder produced at least one event, draining
// the channel so the helper does not block.
func drainEvents(rec *record.FakeRecorder) bool {
	got := false
	for {
		select {
		case e := <-rec.Events:
			_ = e
			got = true
		default:
			return got
		}
	}
}

// shuttingDownSince builds a ShuttingDown Machine for this node whose episode
// began at at.
func shuttingDownSince(at time.Time) *v1alpha1.Machine {
	m := machine("node-a", thisNode, v1alpha1.MachineStateShuttingDown)
	m.Status.ShutdownStartTime = &metav1.Time{Time: at}
	return m
}

// TestReconcilePowersOffEachEpisode: a later ShuttingDown episode — the node came
// back and was drained again — powers off again; only a re-delivery of the same
// episode is skipped.
func TestReconcilePowersOffEachEpisode(t *testing.T) {
	t.Parallel()

	first := time.Now().Truncate(time.Second)
	f := newFixture(t, nil, shuttingDownSince(first))
	f.reconcile(t, "node-a")
	f.reconcile(t, "node-a")

	var m v1alpha1.Machine
	if err := f.r.Get(context.Background(), types.NamespacedName{Name: "node-a"}, &m); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	m.Status.ShutdownStartTime = &metav1.Time{Time: first.Add(time.Hour)}
	if err := f.r.Update(context.Background(), &m); err != nil {
		t.Fatalf("start second episode: %v", err)
	}
	f.reconcile(t, "node-a")

	if *f.calls != 2 {
		t.Errorf("PowerOff calls = %d, want 2 (once per episode)", *f.calls)
	}
}

// TestReconcileSkipsWhenHostBootedAfterRequest: a host that booted after the
// power-off was requested is not powered off again — the agent restarted on a
// node that already came back up — and a PowerOffSkipped Event says why.
func TestReconcileSkipsWhenHostBootedAfterRequest(t *testing.T) {
	t.Parallel()

	requested := time.Now().Truncate(time.Second)
	f := newFixture(t, nil, shuttingDownSince(requested))
	f.r.BootTime = func() (time.Time, error) { return requested.Add(2 * time.Minute), nil }

	f.reconcile(t, "node-a")

	if *f.calls != 0 {
		t.Errorf("PowerOff calls = %d, want 0 on a host that booted after the request", *f.calls)
	}
	select {
	case e := <-f.recorder.Events:
		if !strings.Contains(e, reasonPowerOffSkipped) {
			t.Errorf("event = %q, want %s", e, reasonPowerOffSkipped)
		}
	default:
		t.Errorf("no %s event recorded", reasonPowerOffSkipped)
	}
}

func TestParseBootTime(t *testing.T) {
	got, err := parseBootTime(strings.NewReader("cpu  1 2 3\nbtime 1759622330\nprocesses 42\n"))
	if err != nil {
		t.Fatalf("parseBootTime() error = %v", err)
	}
	if want := time.Unix(1759622330, 0); !got.Equal(want) {
		t.Errorf("parseBootTime() = %v, want %v", got, want)
	}
	if _, err := parseBootTime(strings.NewReader("cpu 1 2 3\n")); err == nil {
		t.Error("parseBootTime() without btime: err = nil, want error")
	}
}

// TestReconcilePowerOffFailureRecordsEvent: a failed power-off leaves a
// PowerOffFailed Warning on the Machine with the command's error, so the operator
// sees why the node did not go down rather than only a later ShutdownTimeout.
func TestReconcilePowerOffFailureRecordsEvent(t *testing.T) {
	t.Parallel()

	f := newFixture(t, errors.New("nsenter: permission denied"), machine("node-a", thisNode, v1alpha1.MachineStateShuttingDown))
	if _, err := f.r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "node-a"},
	}); err == nil {
		t.Fatal("Reconcile() err = nil, want the power-off error")
	}

	select {
	case e := <-f.recorder.Events:
		if !strings.Contains(e, reasonPowerOffFailed) || !strings.Contains(e, "permission denied") {
			t.Errorf("event = %q, want %s carrying the command error", e, reasonPowerOffFailed)
		}
	default:
		t.Errorf("no %s event recorded", reasonPowerOffFailed)
	}
}
