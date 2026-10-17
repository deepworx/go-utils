package scheduler

import (
	"errors"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/deepworx/go-utils/internal/telemetry"
)

type metrics struct {
	duration          metric.Float64Histogram
	candidateFailures metric.Int64Counter
	lastSuccess       metric.Float64Gauge
}

// newMetrics creates the instruments on meter. Instruments that fail to
// create fall back to no-ops, so the returned metrics are always usable.
func newMetrics(meter metric.Meter) (metrics, error) {
	duration, errDuration := telemetry.SchedulerRunDuration.Float64Histogram(meter)
	if errDuration != nil {
		duration = noop.Float64Histogram{}
	}
	failures, errFailures := telemetry.SchedulerRunCandidateFailures.Int64Counter(meter)
	if errFailures != nil {
		failures = noop.Int64Counter{}
	}
	lastSuccess, errLastSuccess := telemetry.SchedulerJobLastSuccess.Float64Gauge(meter)
	if errLastSuccess != nil {
		lastSuccess = noop.Float64Gauge{}
	}
	return metrics{duration: duration, candidateFailures: failures, lastSuccess: lastSuccess},
		errors.Join(errDuration, errFailures, errLastSuccess)
}
