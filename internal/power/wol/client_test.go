package wol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientWake(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		mac              string
		broadcast        string
		status           int
		respBody         string
		wantErr          bool
		wantErrContains  string
		wantBroadcastSet bool
	}{
		{
			name:             "success with explicit broadcast",
			mac:              "01:23:45:67:89:ab",
			broadcast:        "192.168.1.255",
			status:           http.StatusNoContent,
			wantBroadcastSet: true,
		},
		{
			name:      "success with empty broadcast omits field",
			mac:       "01:23:45:67:89:ab",
			broadcast: "",
			status:    http.StatusNoContent,
		},
		{
			name:            "non-2xx surfaces status and body",
			mac:             "01:23:45:67:89:ab",
			broadcast:       "192.168.1.255",
			status:          http.StatusBadGateway,
			respBody:        "send failed: network unreachable",
			wantErr:         true,
			wantErrContains: "network unreachable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotMethod, gotPath string
			var gotReq WakeRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				_ = json.NewDecoder(r.Body).Decode(&gotReq)
				if tt.respBody != "" {
					http.Error(w, tt.respBody, tt.status)
					return
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			c := NewClient(srv.URL, srv.Client())
			err := c.Wake(context.Background(), tt.mac, tt.broadcast)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Wake() err = nil, want error")
				}
				if tt.wantErrContains != "" && !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("Wake() err = %q, want to contain %q", err, tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Wake() unexpected error: %v", err)
			}

			if gotMethod != http.MethodPost {
				t.Errorf("method = %q, want POST", gotMethod)
			}
			if gotPath != "/wake" {
				t.Errorf("path = %q, want /wake", gotPath)
			}
			if gotReq.MacAddress != tt.mac {
				t.Errorf("macAddress = %q, want %q", gotReq.MacAddress, tt.mac)
			}
			if gotReq.BroadcastAddress != tt.broadcast {
				t.Errorf("broadcastAddress = %q, want %q", gotReq.BroadcastAddress, tt.broadcast)
			}
		})
	}
}

// TestClientWakeBaseURLTrailingSlash locks the contract that a trailing slash on
// the base URL does not produce a double slash in the request path.
func TestClientWakeBaseURLTrailingSlash(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/", srv.Client())
	if err := c.Wake(context.Background(), "01:23:45:67:89:ab", ""); err != nil {
		t.Fatalf("Wake() unexpected error: %v", err)
	}
	if gotPath != "/wake" {
		t.Errorf("path = %q, want /wake", gotPath)
	}
}
