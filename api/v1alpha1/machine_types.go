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
