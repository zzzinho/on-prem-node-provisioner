package wol

// WakeRequest is the JSON body the controller POSTs to onp-wol-agent's /wake
// endpoint. The field tags match the WoLConfig CRD keys (api/v1alpha1) so a
// Machine's wol block can be forwarded to the agent without a translation step
// — the agent never imports the CRD types, the wire shape keeps them in sync.
type WakeRequest struct {
	// MacAddress is the target NIC's MAC, e.g. "01:23:45:67:89:ab".
	MacAddress string `json:"macAddress"`
	// BroadcastAddress is the L2 broadcast destination. When empty the agent
	// falls back to 255.255.255.255.
	BroadcastAddress string `json:"broadcastAddress,omitempty"`
}
