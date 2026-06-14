package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MachineSpec is the desired state of a Machine: the identity and power
// metadata an operator supplies for one physical, ONP-managed node.
type MachineSpec struct {
	// NodeName is the name of the Node this Machine maps to. In Phase 1 it is
	// always equal to metadata.name; the field is explicit so the mapping can
	// be relaxed later without a schema change.
	// +kubebuilder:validation:Required
	NodeName string `json:"nodeName"`

	// Capacity is the node's resource capacity used for fit checks while the
	// node is powered off. It is the source of truth precisely because the
	// kubelet-reported Node.Status.Capacity is stale or empty when off.
	// +optional
	Capacity corev1.ResourceList `json:"capacity,omitempty"`

	// Labels are merged with NodePool.template labels and applied to the Node
	// once it becomes Ready.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Power selects and configures how this node is powered on.
	// +kubebuilder:validation:Required
	Power PowerSpec `json:"power"`

	// Shutdown selects how this node is powered off. Phase 1 has a single
	// provider (agent); the field carries no other options yet.
	// +optional
	Shutdown *ShutdownSpec `json:"shutdown,omitempty"`
}

// PowerSpec is a discriminated union of power-on providers, keyed by Provider.
// Exactly one provider-specific block is expected to be set for the selected
// Provider; adding a new provider means adding a new field, not editing the
// existing ones.
type PowerSpec struct {
	// Provider names the power-on mechanism. Phase 1 supports wol only.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=wol
	Provider string `json:"provider"`

	// WoL holds Wake-on-LAN configuration; set when Provider is wol.
	// +optional
	WoL *WoLConfig `json:"wol,omitempty"`
}

// WoLConfig configures the Wake-on-LAN power-on path.
type WoLConfig struct {
	// MacAddress is the target NIC's MAC, e.g. "aa:bb:cc:dd:ee:ff".
	// +kubebuilder:validation:Required
	MacAddress string `json:"macAddress"`

	// BroadcastAddress is the L2 broadcast destination for the magic packet.
	// When empty the controller falls back to 255.255.255.255.
	// +optional
	BroadcastAddress string `json:"broadcastAddress,omitempty"`
}

// ShutdownProviderAgent is the only Phase 1 shutdown provider: the
// onp-shutdown-agent DaemonSet runs systemctl poweroff on the node. A hard-cut
// power provider is a Phase 2 enum extension, not a schema break.
const ShutdownProviderAgent = "agent"

// ShutdownSpec selects how a node is powered off. The provider field exists so
// the union can grow (e.g. a hard-cut power provider) without a schema break.
type ShutdownSpec struct {
	// Provider names the shutdown mechanism. Phase 1 supports agent only.
	// +kubebuilder:validation:Enum=agent
	// +kubebuilder:default=agent
	// +optional
	Provider string `json:"provider,omitempty"`
}

// MachineState is the lifecycle state of a Machine and the single source of
// truth coordinating the controller and the agents.
type MachineState string

const (
	// MachineStateOff means the node is powered off (or not yet observed up).
	MachineStateOff MachineState = "Off"
	// MachineStateBooting means a power-on was issued and the node is expected
	// to come up; the controller is waiting for the Node to report Ready.
	MachineStateBooting MachineState = "Booting"
	// MachineStateReady means the backing Node is Ready and schedulable.
	MachineStateReady MachineState = "Ready"
	// MachineStateDraining means the node is being cordoned and drained.
	MachineStateDraining MachineState = "Draining"
	// MachineStateShuttingDown means drain finished and a power-off is in flight.
	MachineStateShuttingDown MachineState = "ShuttingDown"
	// MachineStateFailed means an operation did not complete unambiguously;
	// any ambiguous outcome lands here rather than silently succeeding.
	MachineStateFailed MachineState = "Failed"
)

// MachineStatus is the observed state of a Machine.
type MachineStatus struct {
	// State is the current lifecycle state.
	// +kubebuilder:validation:Enum=Off;Booting;Ready;Draining;ShuttingDown;Failed
	// +optional
	State MachineState `json:"state,omitempty"`

	// BootStartTime is when the controller issued the power-on that moved this
	// Machine into Booting. It is the source of truth for the boot-timeout
	// check: keeping it as an explicit field (rather than reading a Condition's
	// lastTransitionTime) makes the timeout independent of unrelated condition
	// updates and keeps reconcile idempotent across controller restarts.
	// +optional
	BootStartTime *metav1.Time `json:"bootStartTime,omitempty"`

	// DrainStartTime is when the controller moved this Machine into Draining. It
	// is the source of truth for the drain-timeout check, for the same reasons as
	// BootStartTime: an explicit field (rather than a Condition's
	// lastTransitionTime) keeps the timeout independent of unrelated condition
	// updates and keeps reconcile idempotent across controller restarts.
	// +optional
	DrainStartTime *metav1.Time `json:"drainStartTime,omitempty"`

	// EmptySince is when the scale-down path first observed this Machine's backing
	// Node to be empty — carrying no evictable (non-DaemonSet, non-mirror, running)
	// pods. It anchors the disruption.consolidateAfter timer: the Node must stay
	// empty from this instant for that whole duration before ONP drains it. It is
	// cleared the moment the Node is no longer empty or the Machine leaves Ready,
	// so a node that briefly empties — or is later re-woken — starts a fresh timer
	// rather than draining on a stale observation. Like BootStartTime and
	// DrainStartTime it is an explicit field, not a Condition timestamp, to keep
	// the timer independent of unrelated status churn and idempotent across
	// controller restarts.
	// +optional
	EmptySince *metav1.Time `json:"emptySince,omitempty"`

	// ShutdownStartTime is when the controller moved this Machine into ShuttingDown.
	// It is the source of truth for the shutdown-timeout check: if the backing Node
	// has not gone NotReady within that budget, the power-off did not take effect
	// (e.g. the agent never ran, or the board powered itself back on) and the
	// Machine is failed rather than polling forever. It is an explicit field, not a
	// Condition timestamp, for the same idempotency reasons as BootStartTime and
	// DrainStartTime.
	// +optional
	ShutdownStartTime *metav1.Time `json:"shutdownStartTime,omitempty"`

	// NotReadySince is when the controller first observed a Ready Machine's backing
	// Node go NotReady without ONP having initiated a drain — an external power-off
	// or node loss. It anchors a grace window: only if the Node stays NotReady for
	// that whole window does the Machine fall back to Off (whence scale-up can wake
	// it again), so a brief kubelet blip does not flap the Machine. It is cleared
	// the moment the Node is Ready again. Like EmptySince it is an explicit field,
	// not a Condition timestamp, to keep the timer independent of status churn and
	// idempotent across controller restarts.
	// +optional
	NotReadySince *metav1.Time `json:"notReadySince,omitempty"`

	// Conditions follow the standard Kubernetes condition pattern.
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=mc
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="MAC",type=string,priority=1,JSONPath=`.spec.power.wol.macAddress`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Machine is an individual physical, ONP-managed node: its identity and the
// metadata ONP needs to power it on and off.
type Machine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineSpec   `json:"spec,omitempty"`
	Status MachineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineList is a list of Machine.
type MachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Machine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Machine{}, &MachineList{})
}
