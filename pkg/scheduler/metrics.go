package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/deepworx/go-utils/internal/telemetry"
)

type metrics struct {
	duration          metric.Float64Histogram
	candidateFailures metric.Int64Counter
	// meter and lastSuccess are the leader-only gauge; lastSuccess is nil
	// when it could not be created.
	meter       metric.Meter
	lastSuccess metric.Float64ObservableGauge
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
	lastSuccess, errLastSuccess := telemetry.SchedulerJobLastSuccess.Float64ObservableGauge(meter)
	if errLastSuccess != nil {
		lastSuccess = nil
	}
	return metrics{duration: duration, candidateFailures: failures, meter: meter, lastSuccess: lastSuccess},
		errors.Join(errDuration, errFailures, errLastSuccess)
}

// observeInput is what the leader-only last_success gauge reports.
type observeInput struct {
	jobs []string
	// lastSuccess returns the last successful finish per job.
	lastSuccess func(ctx context.Context, jobs []string) (map[string]time.Time, error)
}

// observe reports the last_success gauge of in.jobs at each collection
// until the returned stop is called. Only the leader calls it, so a pod
// that loses the leader lease stops exporting the series.
func (m metrics) observe(in observeInput) (stop func()) {
	if m.lastSuccess == nil || len(in.jobs) == 0 {
		return func() {}
	}
	reg, err := m.meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		finished, err := in.lastSuccess(ctx, in.jobs)
		if err != nil {
			return fmt.Errorf("scheduler: poll last_success: %w", err)
		}
		for job, at := range finished {
			o.ObserveFloat64(m.lastSuccess, float64(at.Unix()), metric.WithAttributes(telemetry.AttrJob.String(job)))
		}
		return nil
	}, m.lastSuccess)
	if err != nil {
		otel.Handle(fmt.Errorf("scheduler: register last_success gauge: %w", err))
		return func() {}
	}
	return func() {
		if err := reg.Unregister(); err != nil {
			otel.Handle(fmt.Errorf("scheduler: unregister last_success gauge: %w", err))
		}
	}
}
