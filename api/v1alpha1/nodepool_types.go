package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodePoolSpec is the desired state of a NodePool: the policy that groups a set
// of Machines (by label) and bounds how ONP scales them.
//
// M3.0 uses MinNodes (as a stored floor, not yet enforced) and MachineSelector
// (to compute membership and status). The remaining fields are placeholders for
// later milestones — they carry validation and defaults now so the schema does
// not break when those milestones land (see CLAUDE.md "인터페이스 안정성").
type NodePoolSpec struct {
	// MinNodes is the floor of nodes ONP keeps powered on for this pool. M3.0
	// only stores it; the floor is enforced in M5.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinNodes int32 `json:"minNodes"`

	// MaxNodes caps how many nodes the pool may have powered on. A nil pointer
	// means unbounded. Enforced in M3.3; M3.0 only stores it.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxNodes *int32 `json:"maxNodes,omitempty"`

	// MachineSelector selects the Machines that belong to this pool by label.
	// Label selection (rather than ownerReference) lets operators register
	// Machines first and group them later with a one-line label edit; the cost
	// is that a Machine can match two pools, which the controller surfaces as a
	// conflict in later milestones. M3.0 uses this to compute membership.
	// +kubebuilder:validation:Required
	MachineSelector metav1.LabelSelector `json:"machineSelector"`

	// Template holds the labels and taints applied to a Node once its Machine
	// becomes Ready. Consumed in M3.2+; M3.0 only stores it.
	// +optional
	Template NodePoolTemplate `json:"template,omitempty"`

	// Disruption configures when ONP scales the pool down. Consumed in M4;
	// M3.0 only stores it.
	// +optional
	Disruption DisruptionSpec `json:"disruption,omitempty"`

	// Cooldown rate-limits scale decisions. Consumed in M3.3/M5; M3.0 only
	// stores it.
	// +optional
	Cooldown CooldownSpec `json:"cooldown,omitempty"`

	// Drain configures how nodes are drained before power-off. Consumed in M4;
	// M3.0 only stores it.
	// +optional
	Drain DrainSpec `json:"drain,omitempty"`
}

// NodePoolTemplate is the Node-shaping hint applied when a Machine in the pool
// becomes Ready. It is merged with Machine.spec.labels. Consumed in M3.2+.
type NodePoolTemplate struct {
	// Labels are merged onto the backing Node.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Taints are applied to the backing Node.
	// +optional
	Taints []corev1.Taint `json:"taints,omitempty"`
}

// DisruptionSpec configures scale-down policy. Consumed in M4.
type DisruptionSpec struct {
	// ConsolidationPolicy selects when a node may be disrupted. Phase 1 supports
	// WhenEmpty only. WhenUnderutilized is deliberately excluded from the enum
	// until Phase 2 — adding it then is an enum extension, not a schema break.
	// +kubebuilder:validation:Enum=WhenEmpty
	// +optional
	ConsolidationPolicy string `json:"consolidationPolicy,omitempty"`

	// ConsolidateAfter is how long a node must stay empty before it is drained.
	// +optional
	ConsolidateAfter *metav1.Duration `json:"consolidateAfter,omitempty"`
}

// CooldownSpec rate-limits scale decisions. Consumed in M3.3/M5.
type CooldownSpec struct {
	// ScaleUp is the minimum interval between wake decisions.
	// +optional
	ScaleUp *metav1.Duration `json:"scaleUp,omitempty"`

	// ScaleDown is the minimum interval between power-off decisions.
	// +optional
	ScaleDown *metav1.Duration `json:"scaleDown,omitempty"`
}

// DrainSpec configures how a node is drained before power-off. Consumed in M4.
type DrainSpec struct {
	// TimeoutSeconds bounds how long a drain may run before it stops. A nil
	// pointer means the controller default (300s).
	// +kubebuilder:validation:Minimum=0
	// +optional
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`

	// Force, when true, lets the drain evict do-not-disrupt / PDB-blocked Pods
	// after the timeout. It defaults to false: deleting data without an explicit
	// opt-in is exactly the "silently lose data" default ONP refuses to ship.
	// +optional
	Force bool `json:"force,omitempty"`
}

// NodePoolStatus is the observed state of a NodePool: the membership aggregate
// ONP computes from the Machines matching spec.machineSelector.
type NodePoolStatus struct {
	// TotalMachines is the number of Machines matching spec.machineSelector.
	// +optional
	TotalMachines int32 `json:"totalMachines"`

	// ReadyMachines is the number of matching Machines whose status.state is
	// Ready.
	// +optional
	ReadyMachines int32 `json:"readyMachines"`

	// LastScaleUpTime is when ONP most recently woke a Machine in this pool; it
	// anchors cooldown.scaleUp rate-limiting. Nil means no scale-up yet.
	// +optional
	LastScaleUpTime *metav1.Time `json:"lastScaleUpTime,omitempty"`

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
// +kubebuilder:resource:scope=Cluster,shortName=np
// +kubebuilder:printcolumn:name="Min",type=integer,JSONPath=`.spec.minNodes`
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=`.spec.maxNodes`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalMachines`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyMachines`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NodePool is the pool-level policy that groups Machines by label and bounds
// how ONP scales them. It is cluster-scoped, like Machine, so the two read
// consistently with kubectl.
type NodePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodePoolSpec   `json:"spec,omitempty"`
	Status NodePoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NodePoolList is a list of NodePool.
type NodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodePool{}, &NodePoolList{})
}
