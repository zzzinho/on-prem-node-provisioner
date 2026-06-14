package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func TestRecordPowerOn(t *testing.T) {
	PowerOnTotal.Reset()

	RecordPowerOn("wol", nil)
	RecordPowerOn("wol", nil)
	RecordPowerOn("wol", errors.New("agent unreachable"))

	if got := testutil.ToFloat64(PowerOnTotal.WithLabelValues("wol", ResultSuccess)); got != 2 {
		t.Errorf("success count = %v, want 2", got)
	}
	if got := testutil.ToFloat64(PowerOnTotal.WithLabelValues("wol", ResultError)); got != 1 {
		t.Errorf("error count = %v, want 1", got)
	}
}

func TestRecordDrainFailure(t *testing.T) {
	DrainFailureTotal.Reset()

	RecordDrainFailure(ReasonDrainTimeout)
	RecordDrainFailure(ReasonShutdownTimeout)
	RecordDrainFailure(ReasonShutdownTimeout)

	if got := testutil.ToFloat64(DrainFailureTotal.WithLabelValues(ReasonDrainTimeout)); got != 1 {
		t.Errorf("drain_timeout = %v, want 1", got)
	}
	if got := testutil.ToFloat64(DrainFailureTotal.WithLabelValues(ReasonShutdownTimeout)); got != 2 {
		t.Errorf("shutdown_timeout = %v, want 2", got)
	}
}

func TestObserveScaleUpLatency(t *testing.T) {
	before, sumBefore := histState(t)

	ObserveScaleUpLatency(30 * time.Second)
	ObserveScaleUpLatency(45 * time.Second)

	count, sum := histState(t)
	if count-before != 2 {
		t.Errorf("sample count delta = %d, want 2", count-before)
	}
	if got := sum - sumBefore; got != 75 {
		t.Errorf("sample sum delta = %v, want 75 (30+45)", got)
	}
}

// histState reads the histogram's current sample count and sum directly, so a
// test can assert on the delta it caused regardless of prior observations.
func histState(t *testing.T) (count uint64, sum float64) {
	t.Helper()
	var m dto.Metric
	if err := ScaleUpLatencySeconds.Write(&m); err != nil {
		t.Fatalf("write histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}
