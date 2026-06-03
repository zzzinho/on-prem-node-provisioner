package shutdownagent

import (
	"context"
	"errors"
	"testing"

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
