// Package agent implements onp-wol-agent's HTTP surface: a single /wake
// endpoint that turns a WakeRequest into a Wake-on-LAN magic packet.
//
// The package is deliberately k8s-free. onp-wol-agent ships as a scratch image,
// so nothing here may pull in api/v1alpha1 or k8s.io/*; it depends only on the
// pure wol wire types and the standard library.
package agent

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"

	"github.com/zzzinho/on-prem-node-provisioner/internal/power/wol"
)

// defaultBroadcast is used when a WakeRequest omits BroadcastAddress.
const defaultBroadcast = "255.255.255.255"

// Waker sends a magic packet for mac to broadcast. It is the seam between the
// HTTP handler and the actual UDP send: production wraps wol.Send, tests inject
// a fake so /wake can be exercised without touching a socket.
type Waker interface {
	Wake(mac net.HardwareAddr, broadcast string) error
}

// WakerFunc adapts a plain function to the Waker interface.
type WakerFunc func(mac net.HardwareAddr, broadcast string) error

// Wake calls f.
func (f WakerFunc) Wake(mac net.HardwareAddr, broadcast string) error {
	return f(mac, broadcast)
}

// sendWaker is the production Waker: a thin adapter over the M1 wol.Send path.
var sendWaker Waker = WakerFunc(wol.Send)

// Handler returns the /wake HTTP handler. A nil waker uses the real wol.Send;
// a nil logger discards logs. Both are injectable so tests stay hermetic.
func Handler(waker Waker, logger *slog.Logger) http.Handler {
	if waker == nil {
		waker = sendWaker
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req wol.WakeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		mac, err := net.ParseMAC(req.MacAddress)
		if err != nil {
			http.Error(w, "invalid macAddress", http.StatusBadRequest)
			return
		}

		broadcast := req.BroadcastAddress
		if broadcast == "" {
			broadcast = defaultBroadcast
		}

		if err := waker.Wake(mac, broadcast); err != nil {
			// WoL has no ack — a send failure here is the host stack refusing
			// the packet, not the node failing to boot. 502: we are the proxy
			// to that send and it failed upstream of us.
			logger.Error("wake packet send failed",
				slog.String("mac", mac.String()),
				slog.String("broadcast", broadcast),
				slog.Any("error", err),
			)
			http.Error(w, "send failed", http.StatusBadGateway)
			return
		}

		logger.Info("wake packet sent",
			slog.String("mac", mac.String()),
			slog.String("broadcast", broadcast),
		)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
