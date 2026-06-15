package controller

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
)

// poolUnassigned is the onp_nodes_total pool label for a Machine matching no pool.
const poolUnassigned = "<none>"

// StateCollector exposes the two current-state gauges — onp_nodes_total{pool,state}
// and onp_pending_unschedulable — by listing live objects at scrape time rather
// than tracking deltas in reconcilers. Computing at scrape keeps the gauges
// correct without the controller mirroring every state change, and never leaves a
// stale series for a deleted Machine or Pod. It reads through the manager's cache,
// so a scrape is in-memory, not an API round-trip.
type StateCollector struct {
	client               client.Client
	nodesTotal           *prometheus.Desc
	pendingUnschedulable *prometheus.Desc
}

// NewStateCollector returns a collector backed by the manager's cache-reading
// client.
func NewStateCollector(c client.Client) *StateCollector {
	return &StateCollector{
		client: c,
		nodesTotal: prometheus.NewDesc(
			"onp_nodes_total",
			"Number of Machines by pool and lifecycle state.",
			[]string{"pool", "state"}, nil,
		),
		pendingUnschedulable: prometheus.NewDesc(
			"onp_pending_unschedulable",
			"Number of unschedulable pending pods awaiting a node.",
			nil, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *StateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.nodesTotal
	ch <- c.pendingUnschedulable
}

// Collect implements prometheus.Collector. A list error skips that one metric for
// this scrape rather than failing the whole /metrics response, so a transient
// empty cache at startup does not break scraping.
func (c *StateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()
	c.collectNodes(ctx, ch)
	c.collectPending(ctx, ch)
}

func (c *StateCollector) collectNodes(ctx context.Context, ch chan<- prometheus.Metric) {
	var pools v1alpha1.NodePoolList
	if err := c.client.List(ctx, &pools); err != nil {
		log.FromContext(ctx).Error(err, "metrics: list nodepools for onp_nodes_total")
		return
	}
	var machines v1alpha1.MachineList
	if err := c.client.List(ctx, &machines); err != nil {
		log.FromContext(ctx).Error(err, "metrics: list machines for onp_nodes_total")
		return
	}

	type key struct{ pool, state string }
	counts := map[key]int{}
	for i := range machines.Items {
		m := &machines.Items[i]
		state := string(m.Status.State)
		if state == "" {
			state = "Unknown"
		}
		counts[key{firstPoolName(m, pools.Items), state}]++
	}
	for k, v := range counts {
		ch <- prometheus.MustNewConstMetric(c.nodesTotal, prometheus.GaugeValue, float64(v), k.pool, k.state)
	}
}

func (c *StateCollector) collectPending(ctx context.Context, ch chan<- prometheus.Metric) {
	var pods corev1.PodList
	if err := c.client.List(ctx, &pods); err != nil {
		log.FromContext(ctx).Error(err, "metrics: list pods for onp_pending_unschedulable")
		return
	}
	var n int
	for i := range pods.Items {
		if isScaleUpCandidate(&pods.Items[i]) {
			n++
		}
	}
	ch <- prometheus.MustNewConstMetric(c.pendingUnschedulable, prometheus.GaugeValue, float64(n))
}

// firstPoolName returns the name of the first pool whose selector matches the
// Machine's labels, or poolUnassigned. It mirrors poolForMachine's "first match
// wins" over an already-listed pool slice, so the collector lists pools once per
// scrape instead of once per Machine.
func firstPoolName(m *v1alpha1.Machine, pools []v1alpha1.NodePool) string {
	machineLabels := labels.Set(m.Labels)
	for i := range pools {
		selector, err := metav1.LabelSelectorAsSelector(&pools[i].Spec.MachineSelector)
		if err != nil {
			continue
		}
		if selector.Matches(machineLabels) {
			return pools[i].Name
		}
	}
	return poolUnassigned
}
