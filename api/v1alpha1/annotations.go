package v1alpha1

// Annotation keys ONP reads and writes on Machine objects. They live in the API
// package so both the controller and any future client share one spelling.
const (
	// AnnotationWakeNow, set to "true" on a Machine, asks the controller to
	// power the node on. The controller treats the annotation as a one-shot
	// trigger: it removes the annotation once the node reaches Ready so a stale
	// "true" does not re-wake a node the operator later powers down.
	AnnotationWakeNow = "onp.io/wake-now"

	// AnnotationWakeNowValue is the value AnnotationWakeNow must hold to trigger
	// a wake. Any other value (or absence) is treated as "do not wake".
	AnnotationWakeNowValue = "true"
)

// Condition types ONP sets on Machine.status.conditions. They follow the
// standard Kubernetes condition pattern (type, status, lastTransitionTime,
// reason) so operators can read them with kubectl and tooling can watch them.
const (
	// ConditionPowerOnSucceeded reports whether the most recent power-on command
	// was accepted by the provider. True means accepted, not that the node
	// booted — the boot signal is the backing Node going Ready.
	ConditionPowerOnSucceeded = "PowerOnSucceeded"

	// ConditionReady reports whether the backing Node is Ready and schedulable.
	ConditionReady = "Ready"
)
