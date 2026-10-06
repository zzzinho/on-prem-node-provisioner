// Package scheduler answers a single question for ONP's scale-up path: could a
// pending Pod schedule onto a node of a given shape, if that node were powered
// on and Ready? This is a fit *simulation*, not real scheduling — kube-scheduler
// remains the authority. ONP uses it to choose which powered-off Machine to wake.
//
// The target node is OFF, so every rule is a Predicate over the Pod and a
// synthetic Node describing how that node will look once Ready. The defaults:
//
//   - the node is not cordoned (unless the Pod tolerates it),
//   - nodeSelector and required node affinity match the node's labels,
//   - the Pod tolerates the node's NoSchedule / NoExecute taints,
//   - resource requests fit the node's allocatable.
//
// A rule that needs cluster state the package does not hold is a Predicate the
// caller builds and passes to Fit — VolumeNodeAffinity for the node affinity of
// the Pod's bound PersistentVolumes. The room taken by pods still bound to the
// node is the caller's to subtract from the synthetic allocatable.
//
// Still excluded, because they need live placement state or are scoring rather
// than hard predicates: preferred affinity and other scoring, inter-pod affinity
// / anti-affinity, topology spread, host ports, and volume count / CSI limits.
//
// The package operates on core types (*corev1.Pod, *corev1.Node) only, with no
// dependency on ONP's CRD types; assembling a synthetic Node from a Machine is a
// separate concern.
package scheduler

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/component-helpers/resource"
	v1helper "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
)

// Result is the outcome of a fit check. Reason is empty when Fits is true and
// otherwise holds a short, human-readable explanation suitable for an Event or log.
type Result struct {
	Fits   bool
	Reason string
}

// Predicate is one hard scheduling rule. It returns "" when pod may run on node
// and otherwise a short, human-readable reason. A new rule is added as a
// Predicate — to defaultPredicates, or passed to Fit by a caller that holds the
// cluster state the rule needs — never by editing Fit.
type Predicate func(pod *corev1.Pod, node *corev1.Node) string

// defaultPredicates are the rules every fit check applies, in order: the cheap
// label and taint matches before the resource sums.
var defaultPredicates = []Predicate{schedulable, matchesNodeAffinity, toleratesHardTaints, fitsRequests}

// Fit reports whether pod could schedule onto node, considering only the
// predicates ONP can evaluate without live cluster state, plus any extra ones the
// caller supplies. The node is typically synthetic — assembled from a (possibly
// powered-off) Machine's declared capacity, labels, and taints — so
// node.Status.Allocatable is the authoritative capacity.
//
// On the first failing predicate it returns Fits:false with that Reason; all
// passing yields Fits:true with an empty Reason.
func Fit(pod *corev1.Pod, node *corev1.Node, extra ...Predicate) Result {
	for _, predicates := range [][]Predicate{defaultPredicates, extra} {
		for _, p := range predicates {
			if reason := p(pod, node); reason != "" {
				return Result{Reason: reason}
			}
		}
	}
	return Result{Fits: true}
}

// schedulable fails on a cordoned node unless the pod tolerates the unschedulable
// taint (as DaemonSet pods do), mirroring kube-scheduler's NodeUnschedulable
// plugin.
func schedulable(pod *corev1.Pod, node *corev1.Node) string {
	if !node.Spec.Unschedulable {
		return ""
	}
	cordon := corev1.Taint{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule}
	if v1helper.TolerationsTolerateTaint(pod.Spec.Tolerations, &cordon) {
		return ""
	}
	return "node is cordoned"
}

// matchesNodeAffinity checks nodeSelector + required nodeAffinity.
// GetRequiredNodeAffinity folds both into one matcher and handles every operator
// (In/NotIn/Exists/...). Match only errors on a malformed selector, which counts
// as a non-fit.
func matchesNodeAffinity(pod *corev1.Pod, node *corev1.Node) string {
	required := nodeaffinity.GetRequiredNodeAffinity(pod)
	if ok, err := required.Match(node); err != nil || !ok {
		return "node selector / required affinity not satisfied"
	}
	return ""
}

// toleratesHardTaints fails on a NoSchedule or NoExecute taint the pod does not
// tolerate. PreferNoSchedule is a scheduling preference, not a predicate, so the
// filter excludes it.
func toleratesHardTaints(pod *corev1.Pod, node *corev1.Node) string {
	if taint, untolerated := v1helper.FindMatchingUntoleratedTaint(
		node.Spec.Taints,
		pod.Spec.Tolerations,
		isHardTaint,
	); untolerated {
		return fmt.Sprintf("untolerated taint {key=%s effect=%s}", taint.Key, taint.Effect)
	}
	return ""
}

// fitsRequests checks the pod's requests against the node's allocatable.
// PodRequests correctly accounts for init containers, native sidecars
// (restartable init containers), and pod overhead — do not hand-roll.
func fitsRequests(pod *corev1.Pod, node *corev1.Node) string {
	return fitsResources(resource.PodRequests(pod, resource.PodResourcesOptions{}), node.Status.Allocatable)
}

// isHardTaint admits only the taint effects that act as hard scheduling
// predicates. PreferNoSchedule is deliberately excluded.
func isHardTaint(t *corev1.Taint) bool {
	return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
}

// fitsResources returns an empty string if every requested resource fits within
// allocatable, otherwise a reason naming the first offending resource. A resource
// the node does not advertise reads as a zero quantity and therefore fails (e.g.
// a GPU request against a node with no GPUs).
func fitsResources(requests, allocatable corev1.ResourceList) string {
	for name, req := range requests {
		avail := allocatable[name] // absent -> zero-valued Quantity
		if req.Cmp(avail) > 0 {
			return fmt.Sprintf("insufficient %s: requests %s > allocatable %s", name, req.String(), avail.String())
		}
	}
	return ""
}

// VolumeNodeAffinity returns a Predicate that fails unless the node satisfies
// every given PersistentVolume node affinity — the spec.nodeAffinity.required of
// the volumes bound to the pod's claims, which kube-scheduler's VolumeBinding
// plugin enforces. A pod pinned to a local volume on one node must not wake
// another. The caller resolves the pod's volumes; the predicate only matches.
func VolumeNodeAffinity(required []*corev1.NodeSelector) Predicate {
	return func(_ *corev1.Pod, node *corev1.Node) string {
		for _, ns := range required {
			selector, err := nodeaffinity.NewNodeSelector(ns)
			if err != nil || !selector.Match(node) {
				return "volume node affinity conflict"
			}
		}
		return ""
	}
}
