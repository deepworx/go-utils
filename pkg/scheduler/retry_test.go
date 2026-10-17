package scheduler_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/scheduler"
)

func TestTickRetriesAbortedRunAfterRetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		timeout time.Duration
		abort   func(ctx context.Context) error
	}{
		{name: "error", abort: func(context.Context) error { return errors.New("boom") }},
		{name: "panic", abort: func(context.Context) error { panic("boom") }},
		{
			name:    "timeout honoured by the job",
			timeout: 10 * time.Millisecond,
			abort: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
		},
		{
			name:    "timeout ignored by the job",
			timeout: 10 * time.Millisecond,
			abort: func(context.Context) error {
				time.Sleep(20 * time.Millisecond)
				return nil
			},
		},
	}

	start := utc(t, "2026-03-10T08:00:00Z")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newRunningScheduler(t, scheduler.Config{Location: berlin(t), RetryAfter: 5 * time.Minute})
			tick := 0
			var ranOnTicks []int
			mustRegister(t, s, scheduler.Job{
				Name:    "eod",
				Timeout: tt.timeout,
				Run: func(ctx context.Context, r scheduler.Run) (scheduler.Outcome, error) {
					ranOnTicks = append(ranOnTicks, tick)
					if r.Attempt != len(ranOnTicks) {
						t.Errorf("Attempt = %d, want %d", r.Attempt, len(ranOnTicks))
					}
					if r.Attempt == 1 {
						return scheduler.Outcome{}, tt.abort(ctx)
					}
					return scheduler.Outcome{}, nil
				},
			})

			for i, now := range []time.Time{
				start,
				start.Add(4*time.Minute + 59*time.Second),
				start.Add(5*time.Minute + time.Second),
				start.Add(20 * time.Minute),
			} {
				tick = i
				mustTick(t, s, now)
			}

			if want := []int{0, 2}; !slices.Equal(ranOnTicks, want) {
				t.Fatalf("ran on ticks %v, want %v", ranOnTicks, want)
			}
		})
	}
}

func TestTickTakesOverAbandonedRunAfterTimeoutAndRetryAfter(t *testing.T) {
	t.Parallel()

	pool := pgtest.New(t)
	start := utc(t, "2026-03-10T08:00:00Z")
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO scheduler_runs (job, business_date, started_at, attempt) VALUES ('eod', '2026-03-10', $1, 1)`,
		start); err != nil {
		t.Fatalf("insert abandoned run: %v", err)
	}
	s := newRunningSchedulerOn(t, pool, scheduler.Config{Location: berlin(t), RetryAfter: 5 * time.Minute})
	var attempts []int
	mustRegister(t, s, scheduler.Job{
		Name:    "eod",
		Timeout: 10 * time.Minute,
		Run: func(_ context.Context, r scheduler.Run) (scheduler.Outcome, error) {
			attempts = append(attempts, r.Attempt)
			return scheduler.Outcome{}, nil
		},
	})

	mustTick(t, s, start.Add(14*time.Minute+59*time.Second))
	mustTick(t, s, start.Add(15*time.Minute))

	if want := []int{2}; !slices.Equal(attempts, want) {
		t.Fatalf("attempts = %v, want %v", attempts, want)
	}
}

func TestTickRecordsCandidateFailuresWithoutRetry(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	s := newRunningScheduler(t,
		scheduler.Config{Location: berlin(t), RetryAfter: 5 * time.Minute},
		scheduler.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
	)
	runs := 0
	mustRegister(t, s, scheduler.Job{
		Name: "eod",
		Run: func(context.Context, scheduler.Run) (scheduler.Outcome, error) {
			runs++
			return scheduler.Outcome{
				Candidates: 3,
				Failures: []scheduler.Failure{
					{Candidate: "a", Err: errors.New("a failed")},
					{Candidate: "b", Err: errors.New("b failed")},
				},
			}, nil
		},
	})

	start := utc(t, "2026-03-10T08:00:00Z")
	mustTick(t, s, start)
	mustTick(t, s, start.Add(time.Hour))

	if runs != 1 {
		t.Fatalf("runs = %d, want 1", runs)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	if got := int64Sum(t, rm, "scheduler.run.candidate_failures"); got != 2 {
		t.Errorf("scheduler.run.candidate_failures = %d, want 2", got)
	}
	if got := float64Gauge(t, rm, "scheduler.job.last_success"); got < float64(start.Unix()) {
		t.Errorf("scheduler.job.last_success = %v, want >= %d", got, start.Unix())
	}
}

func int64Sum(t *testing.T, rm metricdata.ResourceMetrics, name string) int64 {
	t.Helper()
	sum, ok := findMetric(t, rm, name).Data.(metricdata.Sum[int64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatalf("%s: unexpected data %#v", name, findMetric(t, rm, name).Data)
	}
	return sum.DataPoints[0].Value
}

func float64Gauge(t *testing.T, rm metricdata.ResourceMetrics, name string) float64 {
	t.Helper()
	gauge, ok := findMetric(t, rm, name).Data.(metricdata.Gauge[float64])
	if !ok || len(gauge.DataPoints) != 1 {
		t.Fatalf("%s: unexpected data %#v", name, findMetric(t, rm, name).Data)
	}
	return gauge.DataPoints[0].Value
}

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %s not recorded", name)
	return metricdata.Metrics{}
}
