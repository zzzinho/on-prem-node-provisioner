package power

import (
	"context"
	"fmt"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
	"github.com/zzzinho/on-prem-node-provisioner/internal/power/wol"
)

// providerNameWoL is the Machine.spec.power.provider value this provider serves.
const providerNameWoL = "wol"

// wolProvider adapts a Machine's wol block to the pure wol.Client, bridging the
// k8s-aware controller side to the k8s-free wire/send side. WoL can only power
// on — Capabilities advertises that, and PowerOff/PowerStatus return
// ErrUnsupported per the design's honest-asymmetry rule.
type wolProvider struct {
	client *wol.Client
}

// NewWoLProvider returns a PowerProvider backed by the given agent client.
func NewWoLProvider(client *wol.Client) PowerProvider {
	return &wolProvider{client: client}
}

func (p *wolProvider) Name() string {
	return providerNameWoL
}

func (p *wolProvider) Capabilities() Capabilities {
	return Capabilities{CanPowerOn: true}
}

func (p *wolProvider) PowerOn(ctx context.Context, m *v1alpha1.Machine) error {
	cfg := m.Spec.Power.WoL
	if cfg == nil {
		return fmt.Errorf("power: machine %q has provider wol but no wol config", m.Name)
	}
	if err := p.client.Wake(ctx, cfg.MacAddress, cfg.BroadcastAddress); err != nil {
		return fmt.Errorf("power: wake machine %q: %w", m.Name, err)
	}
	return nil
}

func (p *wolProvider) PowerOff(ctx context.Context, m *v1alpha1.Machine) error {
	return ErrUnsupported
}

func (p *wolProvider) PowerStatus(ctx context.Context, m *v1alpha1.Machine) (State, error) {
	return StateUnknown, ErrUnsupported
}
