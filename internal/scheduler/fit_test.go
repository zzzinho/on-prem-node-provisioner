package scheduler_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/zzzinho/on-prem-node-provisioner/internal/scheduler"
)

// node builds a Ready-shaped node whose Allocatable is the authoritative
// capacity for fit checks (mirroring a synthetic node assembled from a Machine).
func node(allocatable corev1.ResourceList, labels map[string]string, taints []corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: labels},
		Spec:       corev1.NodeSpec{Taints: taints},
		Status:     corev1.NodeStatus{Allocatable: allocatable},
	}
}

// pod builds a single-container pod requesting the given resources. The other
// fields (selector, affinity, tolerations, init/sidecar, overhead) are layered
// on by individual cases.
func pod(requests corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: corev1.ResourceRequirements{Requests: requests},
			}},
		},
	}
}

func list(pairs ...string) corev1.ResourceList {
	if len(pairs)%2 != 0 {
		panic("list: odd number of arguments")
	}
	rl := corev1.ResourceList{}
	for i := 0; i < len(pairs); i += 2 {
		rl[corev1.ResourceName(pairs[i])] = resource.MustParse(pairs[i+1])
	}
	return rl
}

// requiredInLabel builds a pod whose required nodeAffinity demands that the
// given label key be one of values (the In operator).
func requiredInLabel(key string, values ...string) *corev1.Pod {
	p := pod(nil)
	p.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      key,
						Operator: corev1.NodeSelectorOpIn,
						Values:   values,
					}},
				}},
			},
		},
	}
	return p
}

func TestFit(t *testing.T) {
	t.Parallel()

	ample := func() corev1.ResourceList { return list("cpu", "4", "memory", "8Gi") }

	tests := []struct {
		name string
		pod  *corev1.Pod
		node *corev1.Node
		want bool
	}{
		{
			name: "plain pod fits ample node",
			pod:  pod(list("cpu", "500m", "memory", "256Mi")),
			node: node(ample(), nil, nil),
			want: true,
		},
		{
			name: "resource success at exact-equality boundary",
			pod:  pod(list("cpu", "4", "memory", "8Gi")),
			node: node(ample(), nil, nil),
			want: true,
		},
		{
			name: "cpu request exceeds allocatable",
			pod:  pod(list("cpu", "2")),
			node: node(list("cpu", "1"), nil, nil),
			want: false,
		},
		{
			name: "memory request exceeds allocatable",
			pod:  pod(list("memory", "16Gi")),
			node: node(list("memory", "8Gi"), nil, nil),
			want: false,
		},
		{
			name: "requested resource absent on node",
			pod:  pod(list("nvidia.com/gpu", "1")),
			node: node(ample(), nil, nil), // advertises no GPU
			want: false,
		},
		{
			name: "init container drives the effective request",
			pod: func() *corev1.Pod {
				p := pod(list("cpu", "1"))
				// A plain init container runs before app containers; its request is
				// the max with app, so 3 here dominates the 1 of the app container.
				p.Spec.InitContainers = []corev1.Container{{
					Name:      "setup",
					Resources: corev1.ResourceRequirements{Requests: list("cpu", "3")},
				}}
				return p
			}(),
			node: node(list("cpu", "2"), nil, nil),
			want: false, // max(init 3, app 1) = 3 > 2
		},
		{
			name: "native sidecar request adds to app request",
			pod: func() *corev1.Pod {
				p := pod(list("cpu", "1500m"))
				// A restartable init container (native sidecar) runs alongside app
				// containers, so its request is summed, not max'd: 1500m + 1 = 2500m.
				always := corev1.ContainerRestartPolicyAlways
				p.Spec.InitContainers = []corev1.Container{{
					Name:          "sidecar",
					RestartPolicy: &always,
					Resources:     corev1.ResourceRequirements{Requests: list("cpu", "1")},
				}}
				return p
			}(),
			node: node(list("cpu", "2"), nil, nil),
			want: false, // 1500m + 1000m = 2500m > 2000m
		},
		{
			name: "pod overhead adds to the effective request",
			pod: func() *corev1.Pod {
				p := pod(list("cpu", "1"))
				p.Spec.Overhead = list("cpu", "1500m")
				return p
			}(),
			node: node(list("cpu", "2"), nil, nil),
			want: false, // 1 + 1500m = 2500m > 2000m
		},
		{
			name: "nodeSelector matches",
			pod: func() *corev1.Pod {
				p := pod(nil)
				p.Spec.NodeSelector = map[string]string{"disktype": "ssd"}
				return p
			}(),
			node: node(ample(), map[string]string{"disktype": "ssd"}, nil),
			want: true,
		},
		{
			name: "nodeSelector mismatch",
			pod: func() *corev1.Pod {
				p := pod(nil)
				p.Spec.NodeSelector = map[string]string{"disktype": "ssd"}
				return p
			}(),
			node: node(ample(), map[string]string{"disktype": "hdd"}, nil),
			want: false,
		},
		{
			name: "required nodeAffinity In matches",
			pod:  requiredInLabel("zone", "a", "b"),
			node: node(ample(), map[string]string{"zone": "b"}, nil),
			want: true,
		},
		{
			name: "required nodeAffinity In mismatch",
			pod:  requiredInLabel("zone", "a", "b"),
			node: node(ample(), map[string]string{"zone": "c"}, nil),
			want: false,
		},
		{
			name: "untolerated NoSchedule taint fails",
			pod:  pod(nil),
			node: node(ample(), nil, []corev1.Taint{{Key: "gpu", Effect: corev1.TaintEffectNoSchedule}}),
			want: false,
		},
		{
			name: "tolerated NoSchedule taint passes",
			pod: func() *corev1.Pod {
				p := pod(nil)
				p.Spec.Tolerations = []corev1.Toleration{{
					Key:      "gpu",
					Operator: corev1.TolerationOpExists,
					Effect:   corev1.TaintEffectNoSchedule,
				}}
				return p
			}(),
			node: node(ample(), nil, []corev1.Taint{{Key: "gpu", Effect: corev1.TaintEffectNoSchedule}}),
			want: true,
		},
		{
			name: "PreferNoSchedule taint is ignored",
			pod:  pod(nil), // no toleration
			node: node(ample(), nil, []corev1.Taint{{Key: "spot", Effect: corev1.TaintEffectPreferNoSchedule}}),
			want: true,
		},
		{
			name: "passes every predicate together",
			pod: func() *corev1.Pod {
				p := requiredInLabel("zone", "a")
				p.Spec.Containers[0].Resources.Requests = list("cpu", "2", "memory", "4Gi")
				p.Spec.NodeSelector = map[string]string{"disktype": "ssd"}
				p.Spec.Tolerations = []corev1.Toleration{{
					Key:      "dedicated",
					Operator: corev1.TolerationOpExists,
					Effect:   corev1.TaintEffectNoSchedule,
				}}
				return p
			}(),
			node: node(
				ample(),
				map[string]string{"zone": "a", "disktype": "ssd"},
				[]corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoSchedule}},
			),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := scheduler.Fit(tt.pod, tt.node)
			if got.Fits != tt.want {
				t.Fatalf("Fit() = %+v, want Fits=%v", got, tt.want)
			}
			if tt.want && got.Reason != "" {
				t.Errorf("Fit() succeeded but Reason = %q, want empty", got.Reason)
			}
			if !tt.want && strings.TrimSpace(got.Reason) == "" {
				t.Errorf("Fit() failed but Reason is empty; want a non-empty explanation")
			}
		})
	}
}
