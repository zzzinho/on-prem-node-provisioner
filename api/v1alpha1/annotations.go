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

	// AnnotationDrainNow, set to "true" on a Ready Machine, asks the controller
	// to cordon and drain the node, then move it toward power-off. It mirrors
	// AnnotationWakeNow: the controller treats it as a one-shot trigger and
	// removes it once the drain starts, so a stale "true" does not re-drain a
	// node the operator later wakes. Manual operators and the automatic
	// ScaleDownReconciler (M4.2) set it on the same path.
	AnnotationDrainNow = "onp.io/drain-now"

	// AnnotationDrainNowValue is the value AnnotationDrainNow must hold to
	// trigger a drain. Any other value (or absence) is treated as "do not drain".
	AnnotationDrainNowValue = "true"

	// AnnotationCordonedByONP marks a Node that ONP cordoned itself during a
	// drain, distinguishing it from a Node an operator cordoned by hand. ONP sets
	// it together with spec.unschedulable when cordoning and removes it when
	// uncordoning, so the wake path can uncordon only the nodes ONP cordoned and
	// leave an operator's manual cordon untouched. Phase 1 nodes are long-lived
	// and reused, so a scale-down cordon must be lifted when the node is woken
	// back into service or a node woken for a pending pod stays unschedulable.
	AnnotationCordonedByONP = "onp.io/cordoned-by-onp"

	// AnnotationDoNotDisrupt, set to "true", exempts a workload from ONP's
	// disruption. On a Node it removes the whole node from automatic scale-down —
	// ONP never starts its empty timer. On a Pod it protects that pod: the node it
	// runs on is never auto-targeted for scale-down (the pod keeps the node
	// non-empty), and a manual drain will not evict it unless drain.force is set —
	// in which case the drain stalls and times out into Failed rather than
	// disrupting the pod, the "조용히 데이터를 잃지 않는다" default. It mirrors
	// Karpenter's karpenter.sh/do-not-disrupt.
	AnnotationDoNotDisrupt = "onp.io/do-not-disrupt"

	// AnnotationDoNotDisruptValue is the value AnnotationDoNotDisrupt must hold to
	// take effect. Any other value (or absence) means "may disrupt".
	AnnotationDoNotDisruptValue = "true"
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

	// ConditionDrainSucceeded reports the outcome of the most recent drain. True
	// means the node drained cleanly and is moving to ShuttingDown; False means
	// the drain hit its timeout and the Machine was failed (the node is
	// uncordoned), per the "stop, don't force" default.
	ConditionDrainSucceeded = "DrainSucceeded"
)
