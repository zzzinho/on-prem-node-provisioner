package agent

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzzinho/on-prem-node-provisioner/internal/power/wol"
)

func TestHandlerWake(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		method        string
		body          string
		wakeErr       error
		wantStatus    int
		wantCalled    bool
		wantMac       string
		wantBroadcast string
	}{
		{
			name:          "valid request sends with explicit broadcast",
			method:        http.MethodPost,
			body:          `{"macAddress":"01:23:45:67:89:ab","broadcastAddress":"192.168.1.255"}`,
			wantStatus:    http.StatusNoContent,
			wantCalled:    true,
			wantMac:       "01:23:45:67:89:ab",
			wantBroadcast: "192.168.1.255",
		},
		{
			name:          "empty broadcast defaults to 255.255.255.255",
			method:        http.MethodPost,
			body:          `{"macAddress":"01:23:45:67:89:ab"}`,
			wantStatus:    http.StatusNoContent,
			wantCalled:    true,
			wantMac:       "01:23:45:67:89:ab",
			wantBroadcast: defaultBroadcast,
		},
		{
			name:       "malformed JSON is 400",
			method:     http.MethodPost,
			body:       `{not json`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid MAC is 400",
			method:     http.MethodPost,
			body:       `{"macAddress":"not-a-mac"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "non-POST is 405",
			method:     http.MethodGet,
			body:       "",
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:          "send failure is 502",
			method:        http.MethodPost,
			body:          `{"macAddress":"01:23:45:67:89:ab"}`,
			wakeErr:       errors.New("network unreachable"),
			wantStatus:    http.StatusBadGateway,
			wantCalled:    true,
			wantMac:       "01:23:45:67:89:ab",
			wantBroadcast: defaultBroadcast,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotMac net.HardwareAddr
			var gotBroadcast string
			called := false
			waker := WakerFunc(func(mac net.HardwareAddr, broadcast string) error {
				called = true
				gotMac = mac
				gotBroadcast = broadcast
				return tt.wakeErr
			})

			srv := httptest.NewServer(Handler(waker, nil, ""))
			defer srv.Close()

			req, err := http.NewRequest(tt.method, srv.URL+"/wake", strings.NewReader(tt.body))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if called != tt.wantCalled {
				t.Fatalf("waker called = %v, want %v", called, tt.wantCalled)
			}
			if !tt.wantCalled {
				return
			}
			if got := gotMac.String(); got != tt.wantMac {
				t.Errorf("mac = %q, want %q", got, tt.wantMac)
			}
			if gotBroadcast != tt.wantBroadcast {
				t.Errorf("broadcast = %q, want %q", gotBroadcast, tt.wantBroadcast)
			}
		})
	}
}

// TestHandlerAuth locks the shared-token contract: with a token configured,
// /wake demands a matching Bearer header (401 otherwise) while the health
// endpoints stay open for the kubelet's probes.
func TestHandlerAuth(t *testing.T) {
	t.Parallel()

	const token = "test-token-123"
	validBody := `{"macAddress":"01:23:45:67:89:ab"}`

	tests := []struct {
		name       string
		auth       string // Authorization header; empty means absent
		wantStatus int
		wantCalled bool
	}{
		{
			name:       "matching token is accepted",
			auth:       "Bearer " + token,
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "wrong token is 401",
			auth:       "Bearer wrong-token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing header is 401",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "bare token without Bearer scheme is 401",
			auth:       token,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			called := false
			waker := WakerFunc(func(net.HardwareAddr, string) error {
				called = true
				return nil
			})
			srv := httptest.NewServer(Handler(waker, nil, token))
			defer srv.Close()

			req, err := http.NewRequest(http.MethodPost, srv.URL+"/wake", strings.NewReader(validBody))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if called != tt.wantCalled {
				t.Errorf("waker called = %v, want %v", called, tt.wantCalled)
			}
		})
	}

	// Health endpoints must not require the token: the kubelet's probes carry
	// no Authorization header.
	t.Run("health endpoints skip auth", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(Handler(WakerFunc(func(net.HardwareAddr, string) error { return nil }), nil, token))
		defer srv.Close()

		for _, path := range []string{"/healthz", "/readyz"} {
			resp, err := srv.Client().Get(srv.URL + path)
			if err != nil {
				t.Fatalf("get %s: %v", path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s status = %d, want %d", path, resp.StatusCode, http.StatusOK)
			}
		}
	})
}

// TestHandlerBodyLimit checks an oversized /wake body is rejected with 413
// instead of being read to completion.
func TestHandlerBodyLimit(t *testing.T) {
	t.Parallel()

	called := false
	srv := httptest.NewServer(Handler(WakerFunc(func(net.HardwareAddr, string) error {
		called = true
		return nil
	}), nil, ""))
	defer srv.Close()

	huge := `{"macAddress":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	resp, err := srv.Client().Post(srv.URL+"/wake", "application/json", strings.NewReader(huge))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
	if called {
		t.Error("waker called for oversized body")
	}
}

// TestHandlerAllowHeader checks the 405 path advertises POST, per RFC 7231.
func TestHandlerAllowHeader(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(Handler(WakerFunc(func(net.HardwareAddr, string) error { return nil }), nil, ""))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/wake")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want %q", got, http.MethodPost)
	}
}

// TestHandlerHealth checks /healthz and /readyz return 200 on GET (so the
// kubelet's probes pass) and 405 on other methods.
func TestHandlerHealth(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(Handler(WakerFunc(func(net.HardwareAddr, string) error { return nil }), nil, ""))
	defer srv.Close()

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, resp.StatusCode, http.StatusOK)
		}

		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		pr, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		pr.Body.Close()
		if pr.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s status = %d, want %d", path, pr.StatusCode, http.StatusMethodNotAllowed)
		}
	}
}

// guard ensures wol.Send keeps the signature the production Waker adapts.
var _ = WakerFunc(wol.Send)
