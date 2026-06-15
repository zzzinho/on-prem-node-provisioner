// Package power is the controller-side power-control abstraction: the
// PowerProvider interface, the static Capabilities a provider advertises, and a
// Registry that maps provider names to implementations.
//
// This package is k8s-aware (it speaks api/v1alpha1.Machine) and is used only
// by onp-controller. The agent-facing wire and send code lives in the pure
// internal/power/wol package, which this package wraps but the agent imports on
// its own.
package power

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

// ErrUnsupported is returned by a provider method whose capability it does not
// advertise — e.g. WoL's PowerOff. Callers gate on Capabilities first; a method
// returning ErrUnsupported is the honest fallback if they do not.
var ErrUnsupported = errors.New("power: operation not supported by provider")

// State is a provider's view of a node's raw power level — what a BMC or chassis
// can report — deliberately distinct from v1alpha1.MachineState, the controller's
// lifecycle (Booting/Ready/Draining/...). A power provider knows only whether the
// board has power, not where the node is in its Kubernetes lifecycle; returning
// the lifecycle enum here would force every future provider (IPMI, Redfish) into a
// dishonest mapping. The controller interprets State into a MachineState itself.
type State string

const (
	// StateOn means the provider observed the node powered on.
	StateOn State = "On"
	// StateOff means the provider observed the node powered off.
	StateOff State = "Off"
	// StateUnknown means the provider cannot determine the power level — the
	// honest answer for a provider whose Capabilities.CanQueryStatus is false.
	StateUnknown State = "Unknown"
)

// Capabilities declares, statically, which operations a provider can perform.
// Callers branch on these rather than assuming every method works: WoL can only
// power on, so its CanPowerOff/CanQueryStatus are false. The set is static by
// design — a provider's abilities do not change at runtime, so no probing.
type Capabilities struct {
	CanPowerOn     bool
	CanPowerOff    bool
	CanQueryStatus bool
}

// PowerProvider controls the power of one Machine. The interface is symmetric —
// every provider has all four methods — and asymmetry (WoL has no PowerOff) is
// expressed through Capabilities, not through which methods exist. This keeps
// registration, lookup, and logging uniform across providers.
type PowerProvider interface {
	// Name is the provider's stable identifier, matching Machine.spec.power.provider.
	Name() string
	// PowerOn issues a power-on for the Machine. Success means the command was
	// accepted, not that the node booted — Node Ready is observed elsewhere.
	PowerOn(ctx context.Context, m *v1alpha1.Machine) error
	// PowerOff issues a hard power-off. Phase 1 powers nodes off via the
	// shutdown-agent instead, so this exists for future hard-cut providers.
	PowerOff(ctx context.Context, m *v1alpha1.Machine) error
	// PowerStatus reports the provider's view of the node's raw power level (On/
	// Off/Unknown), not its Kubernetes lifecycle. A provider that cannot query
	// returns StateUnknown and ErrUnsupported.
	PowerStatus(ctx context.Context, m *v1alpha1.Machine) (State, error)
	// Capabilities reports which of the above this provider actually supports.
	Capabilities() Capabilities
}

// Registry maps provider names to implementations. A controller builds one at
// startup, registers each provider, and looks one up per Machine by its
// spec.power.provider. The zero value is not usable; call NewRegistry.
type Registry struct {
	providers map[string]PowerProvider
}

// NewRegistry returns an empty Registry ready for Register.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]PowerProvider)}
}

// Register adds p under p.Name(). A duplicate name is an error rather than a
// panic: provider wiring is ordinary startup logic, and returning an error lets
// the caller report which provider collided and exit cleanly — panicking would
// be reserved for truly impossible states, which a name clash is not.
func (r *Registry) Register(p PowerProvider) error {
	name := p.Name()
	if _, ok := r.providers[name]; ok {
		return fmt.Errorf("power: provider %q already registered", name)
	}
	r.providers[name] = p
	return nil
}

// Get returns the provider registered under name and whether it was found.
func (r *Registry) Get(name string) (PowerProvider, bool) {
	p, ok := r.providers[name]
	return p, ok
}
