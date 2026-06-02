// Package scheduler answers a single question for ONP's scale-up path: could a
// pending Pod schedule onto a node of a given shape, if that node were powered
// on and Ready? This is a fit *simulation*, not real scheduling — kube-scheduler
// remains the authority. ONP uses it to choose which powered-off Machine to wake.
//
// Only predicates decidable without live cluster state are evaluated, because the
// target node is OFF (zero running pods, a stale or empty kubelet Node.Status):
//
//   - resource requests fit the node's allocatable,
//   - nodeSelector and required node affinity match the node's labels,
//   - the Pod tolerates the node's NoSchedule / NoExecute taints.
//
// The following are intentionally excluded in Phase 1 because they need live
// cluster state or are scoring rather than hard predicates: preferred affinity
// and other scoring, inter-pod affinity / anti-affinity, topology spread, host
// ports, volume / CSI limits, and the resource usage of already-running pods.
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

// Fit reports whether pod could schedule onto node, considering only the
// predicates ONP can evaluate without live cluster state: resource requests,
// nodeSelector + required node affinity, and taint toleration. The node is
// typically synthetic — assembled from a (possibly powered-off) Machine's
// declared capacity, labels, and taints — so node.Status.Allocatable is the
// authoritative capacity.
//
// Predicates are checked label/affinity, taints, then resources; on the first
// failure it returns Fits:false with a specific Reason. All passing yields
// Fits:true with an empty Reason.
func Fit(pod *corev1.Pod, node *corev1.Node) Result {
	// nodeSelector + required nodeAffinity. GetRequiredNodeAffinity folds both
	// into one matcher and handles every operator (In/NotIn/Exists/...). Match
	// only errors on a malformed selector, which we treat as a non-fit.
	required := nodeaffinity.GetRequiredNodeAffinity(pod)
	if ok, err := required.Match(node); err != nil || !ok {
		return Result{Reason: "node selector / required affinity not satisfied"}
	}

	// Taints: a NoSchedule or NoExecute taint the pod does not tolerate is a hard
	// failure. PreferNoSchedule is a scheduling preference, not a predicate, so the
	// filter excludes it.
	if taint, untolerated := v1helper.FindMatchingUntoleratedTaint(
		node.Spec.Taints,
		pod.Spec.Tolerations,
		isHardTaint,
	); untolerated {
		return Result{Reason: fmt.Sprintf("untolerated taint {key=%s effect=%s}", taint.Key, taint.Effect)}
	}

	// Resources: PodRequests correctly accounts for init containers, native
	// sidecars (restartable init containers), and pod overhead — do not hand-roll.
	requests := resource.PodRequests(pod, resource.PodResourcesOptions{})
	if reason := fitsResources(requests, node.Status.Allocatable); reason != "" {
		return Result{Reason: reason}
	}

	return Result{Fits: true}
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
