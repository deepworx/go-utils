package scheduler_test

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/deepworx/go-utils/pkg/scheduler"
)

func TestLastSuccessStopsWhenLeaderLeaseEnds(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	s := newRunningScheduler(t,
		scheduler.Config{Location: berlin(t)},
		scheduler.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
	)
	mustRegister(t, s, scheduler.Job{
		Name: "eod",
		Run: func(context.Context, scheduler.Run) (scheduler.Outcome, error) {
			return scheduler.Outcome{}, nil
		},
	})
	mustTick(t, s, utc(t, "2026-03-10T08:00:00Z"))
	if !reportsLastSuccess(t, reader) {
		t.Fatal("scheduler.job.last_success not reported by the leader")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if reportsLastSuccess(t, reader) {
		t.Error("scheduler.job.last_success still reported after the leader lease was released")
	}
}

func reportsLastSuccess(t *testing.T, reader *sdkmetric.ManualReader) bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if g, ok := m.Data.(metricdata.Gauge[float64]); ok && m.Name == "scheduler.job.last_success" {
				return len(g.DataPoints) > 0
			}
		}
	}
	return false
}

func TestRunTicksOnClock(t *testing.T) {
	t.Parallel()

	now := utc(t, "2026-03-10T08:00:00Z")
	s := newRunningScheduler(t,
		scheduler.Config{Location: berlin(t), PollInterval: 10 * time.Millisecond},
		scheduler.WithClock(func() time.Time { return now }),
	)
	dates := make(chan string, 1)
	mustRegister(t, s, scheduler.Job{
		Name: "eod",
		Run: func(_ context.Context, r scheduler.Run) (scheduler.Outcome, error) {
			dates <- r.Date.String()
			return scheduler.Outcome{}, nil
		},
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() error = %v", err)
		}
	}()

	select {
	case got := <-dates:
		if got != "2026-03-10" {
			t.Errorf("Run.Date = %s, want 2026-03-10 from the clock", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("job did not run")
	}
}
