// Package metrics holds ONP's Prometheus metrics: the event-driven counters and
// histogram the controller increments as it works, plus the reasons it labels
// them with. All names carry the onp_ prefix (CLAUDE.md) and are registered on
// controller-runtime's shared registry, so they appear on the manager's existing
// /metrics endpoint with no extra server.
//
// The two current-state gauges (onp_nodes_total, onp_pending_unschedulable) are
// not here: they are computed at scrape time by a Collector that lists live
// objects, which lives in internal/controller because it reuses that package's
// pool-membership and unschedulable predicates.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Result label values for outcome-counted operations.
const (
	ResultSuccess = "success"
	ResultError   = "error"
)

// Drain-failure reasons. Kept as constants so the label values operators alert on
// stay stable, matching the Event reasons on the Machine.
const (
	ReasonDrainTimeout    = "drain_timeout"
	ReasonShutdownTimeout = "shutdown_timeout"
)

var (
	// PowerOnTotal counts power-on commands issued, split by provider and whether
	// the provider accepted the command. It counts acceptance, not boot success —
	// the boot signal is the Node going Ready, captured by ScaleUpLatencySeconds.
	PowerOnTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "onp_power_on_total",
			Help: "Count of power-on commands issued, by provider and result (success|error).",
		},
		[]string{"provider", "result"},
	)

	// ScaleUpLatencySeconds measures the wake path's wall-clock cost: from the
	// power-on that moved a Machine into Booting to its backing Node reporting
	// Ready. Buckets span a few seconds (cached BIOS) to ten minutes (cold boot).
	ScaleUpLatencySeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "onp_scale_up_latency_seconds",
			Help:    "Seconds from power-on to the backing Node reporting Ready.",
			Buckets: []float64{5, 10, 15, 20, 30, 45, 60, 90, 120, 180, 300, 600},
		},
	)

	// DrainFailureTotal counts drains and power-offs that did not complete cleanly,
	// by reason (drain_timeout|shutdown_timeout), so an operator can alert on nodes
	// that will not retire.
	DrainFailureTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "onp_drain_failure_total",
			Help: "Count of drains/power-offs that failed, by reason.",
		},
		[]string{"reason"},
	)
)

func init() {
	// Register on controller-runtime's shared registry so the metrics surface on
	// the manager's existing /metrics endpoint.
	ctrlmetrics.Registry.MustRegister(PowerOnTotal, ScaleUpLatencySeconds, DrainFailureTotal)
}

// RecordPowerOn counts one power-on attempt, mapping a nil error to success.
func RecordPowerOn(provider string, err error) {
	result := ResultSuccess
	if err != nil {
		result = ResultError
	}
	PowerOnTotal.WithLabelValues(provider, result).Inc()
}

// ObserveScaleUpLatency records how long a wake took from power-on to Node Ready.
func ObserveScaleUpLatency(d time.Duration) {
	ScaleUpLatencySeconds.Observe(d.Seconds())
}

// RecordDrainFailure counts one failed drain/power-off under the given reason.
func RecordDrainFailure(reason string) {
	DrainFailureTotal.WithLabelValues(reason).Inc()
}
