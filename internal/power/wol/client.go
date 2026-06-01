package wol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultTimeout bounds a single Wake call. The agent's work is one UDP send,
// so the request returns almost immediately; the timeout exists only to keep a
// stuck connection from pinning a reconcile.
const defaultTimeout = 10 * time.Second

// errBodyLimit caps how much of a non-2xx response body we read into an error
// message — enough to surface the agent's reason, small enough not to log a
// runaway page.
const errBodyLimit = 512

// Client talks to one onp-wol-agent over HTTP. It carries no k8s types so the
// agent-side wire code stays a pure, stdlib-only package shared by both ends.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a Client targeting baseURL (e.g. "http://10.0.0.1:9119").
// A nil httpClient gets a default with a bounded timeout; pass your own to
// share a transport or tune dialing.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient,
	}
}

// Wake asks the agent to broadcast a magic packet for macAddress. An empty
// broadcastAddress lets the agent apply its own default. A non-2xx response is
// an error that includes a slice of the response body for diagnosis.
//
// Wake reports only that the agent accepted the send — not that the node woke.
// Per the design, the success signal is Node Ready, observed elsewhere.
func (c *Client) Wake(ctx context.Context, macAddress, broadcastAddress string) error {
	body, err := json.Marshal(WakeRequest{
		MacAddress:       macAddress,
		BroadcastAddress: broadcastAddress,
	})
	if err != nil {
		return fmt.Errorf("wol: marshal wake request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/wake", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("wol: build wake request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("wol: POST wake: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		return fmt.Errorf("wol: agent returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return nil
}
