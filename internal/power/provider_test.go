package power

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
	"github.com/zzzinho/on-prem-node-provisioner/internal/power/wol"
)

func machineWithWoL(mac, broadcast string) *v1alpha1.Machine {
	m := &v1alpha1.Machine{}
	m.Name = "node-a"
	m.Spec.Power.Provider = "wol"
	if mac != "" || broadcast != "" {
		m.Spec.Power.WoL = &v1alpha1.WoLConfig{
			MacAddress:       mac,
			BroadcastAddress: broadcast,
		}
	}
	return m
}

func TestWoLProviderCapabilities(t *testing.T) {
	t.Parallel()

	p := NewWoLProvider(wol.NewClient("http://unused", nil))

	if got := p.Name(); got != "wol" {
		t.Errorf("Name() = %q, want %q", got, "wol")
	}
	want := Capabilities{CanPowerOn: true}
	if got := p.Capabilities(); got != want {
		t.Errorf("Capabilities() = %+v, want %+v", got, want)
	}
}

func TestWoLProviderUnsupported(t *testing.T) {
	t.Parallel()

	p := NewWoLProvider(wol.NewClient("http://unused", nil))
	m := machineWithWoL("01:23:45:67:89:ab", "")

	if err := p.PowerOff(context.Background(), m); !errors.Is(err, ErrUnsupported) {
		t.Errorf("PowerOff() err = %v, want ErrUnsupported", err)
	}
	state, err := p.PowerStatus(context.Background(), m)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("PowerStatus() err = %v, want ErrUnsupported", err)
	}
	if state != "" {
		t.Errorf("PowerStatus() state = %q, want empty", state)
	}
}

func TestWoLProviderPowerOn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		machine   *v1alpha1.Machine
		wantErr   bool
		wantWake  bool
		wantMac   string
		wantBcast string
	}{
		{
			name:      "wakes via client",
			machine:   machineWithWoL("01:23:45:67:89:ab", "192.168.1.255"),
			wantWake:  true,
			wantMac:   "01:23:45:67:89:ab",
			wantBcast: "192.168.1.255",
		},
		{
			name:    "nil wol config errors before any send",
			machine: machineWithWoL("", ""),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotReq wol.WakeRequest
			waked := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				waked = true
				_ = json.NewDecoder(r.Body).Decode(&gotReq)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			p := NewWoLProvider(wol.NewClient(srv.URL, srv.Client()))
			err := p.PowerOn(context.Background(), tt.machine)

			if (err != nil) != tt.wantErr {
				t.Fatalf("PowerOn() err = %v, wantErr %v", err, tt.wantErr)
			}
			if waked != tt.wantWake {
				t.Fatalf("client called = %v, want %v", waked, tt.wantWake)
			}
			if !tt.wantWake {
				return
			}
			if gotReq.MacAddress != tt.wantMac {
				t.Errorf("macAddress = %q, want %q", gotReq.MacAddress, tt.wantMac)
			}
			if gotReq.BroadcastAddress != tt.wantBcast {
				t.Errorf("broadcastAddress = %q, want %q", gotReq.BroadcastAddress, tt.wantBcast)
			}
		})
	}
}

func TestRegistry(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	p := NewWoLProvider(wol.NewClient("http://unused", nil))

	if err := r.Register(p); err != nil {
		t.Fatalf("Register() unexpected error: %v", err)
	}
	if err := r.Register(p); err == nil {
		t.Errorf("Register() duplicate err = nil, want error")
	}

	got, ok := r.Get("wol")
	if !ok {
		t.Fatalf("Get(\"wol\") ok = false, want true")
	}
	if got.Name() != "wol" {
		t.Errorf("Get(\"wol\").Name() = %q, want %q", got.Name(), "wol")
	}
	if _, ok := r.Get("ipmi"); ok {
		t.Errorf("Get(\"ipmi\") ok = true, want false")
	}
}
