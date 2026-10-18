package scheduler_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/scheduler"
)

func TestTickRunsIntervalJobOnFixedGrid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		every time.Duration
		ticks []string
		want  []string
	}{
		{
			name:  "first start mid-interval runs once and then aligns to the grid",
			every: 15 * time.Minute,
			ticks: []string{"2026-03-10T08:07:00Z", "2026-03-10T08:14:59Z", "2026-03-10T08:15:00Z", "2026-03-10T08:29:00Z", "2026-03-10T08:31:00Z"},
			want:  []string{"2026-03-10T08:00:00Z", "2026-03-10T08:15:00Z", "2026-03-10T08:30:00Z"},
		},
		{
			name:  "long gap yields one run and then the next grid point",
			every: 15 * time.Minute,
			ticks: []string{"2026-03-10T08:00:00Z", "2026-03-10T13:47:00Z", "2026-03-10T13:59:59Z", "2026-03-10T14:00:00Z"},
			want:  []string{"2026-03-10T08:00:00Z", "2026-03-10T13:45:00Z", "2026-03-10T14:00:00Z"},
		},
		{
			name:  "grid is on Unix time for intervals that do not divide a day",
			every: 7 * time.Hour,
			ticks: []string{"2026-03-10T08:00:00Z", "2026-03-10T12:59:59Z", "2026-03-10T13:00:00Z"},
			want:  []string{"2026-03-10T06:00:00Z", "2026-03-10T13:00:00Z"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			loc := berlin(t)
			s := newRunningScheduler(t, scheduler.Config{Location: loc})
			var got []string
			mustRegister(t, s, scheduler.Job{
				Name:  "sweep",
				Every: tt.every,
				Run: func(_ context.Context, r scheduler.Run) (scheduler.Outcome, error) {
					if r.Attempt != 1 {
						t.Errorf("Attempt = %d, want 1", r.Attempt)
					}
					if want := scheduler.DateOf(r.Due.In(loc)); r.Date != want {
						t.Errorf("Date = %s, want %s", r.Date, want)
					}
					got = append(got, r.Due.UTC().Format(time.RFC3339))
					return scheduler.Outcome{}, nil
				},
			})

			for _, now := range tt.ticks {
				mustTick(t, s, utc(t, now))
			}

			if !slices.Equal(got, tt.want) {
				t.Fatalf("runs due at %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTickKeepsIntervalGridAcrossRestart(t *testing.T) {
	t.Parallel()

	pool := pgtest.New(t)
	var got []string
	job := scheduler.Job{
		Name:  "sweep",
		Every: 30 * time.Minute,
		Run: func(_ context.Context, r scheduler.Run) (scheduler.Outcome, error) {
			got = append(got, r.Due.UTC().Format(time.RFC3339))
			return scheduler.Outcome{}, nil
		},
	}
	before := newRunningSchedulerOn(t, pool, scheduler.Config{Location: berlin(t)})
	mustRegister(t, before, job)
	mustTick(t, before, utc(t, "2026-03-10T08:00:00Z"))

	after := newRunningSchedulerOn(t, pool, scheduler.Config{Location: berlin(t)})
	mustRegister(t, after, job)
	mustTick(t, after, utc(t, "2026-03-10T08:20:00Z"))
	mustTick(t, after, utc(t, "2026-03-10T08:30:00Z"))

	if want := []string{"2026-03-10T08:00:00Z", "2026-03-10T08:30:00Z"}; !slices.Equal(got, want) {
		t.Fatalf("runs due at %v, want %v", got, want)
	}
}

func TestTickRetriesAbortedIntervalRun(t *testing.T) {
	t.Parallel()

	type run struct {
		due     string
		attempt int
	}
	tests := []struct {
		name       string
		every      time.Duration
		retryAfter time.Duration
		ticks      []string
		want       []run
	}{
		{
			name:       "after RetryAfter when shorter than Every",
			every:      time.Hour,
			retryAfter: 5 * time.Minute,
			ticks:      []string{"2026-03-10T08:00:00Z", "2026-03-10T08:04:59Z", "2026-03-10T08:05:01Z", "2026-03-10T08:30:00Z", "2026-03-10T09:00:00Z"},
			want:       []run{{"2026-03-10T08:00:00Z", 1}, {"2026-03-10T08:00:00Z", 2}, {"2026-03-10T09:00:00Z", 1}},
		},
		{
			name:       "after Every when shorter than RetryAfter",
			every:      10 * time.Minute,
			retryAfter: time.Hour,
			ticks:      []string{"2026-03-10T08:00:00Z", "2026-03-10T08:09:59Z", "2026-03-10T08:10:00Z", "2026-03-10T08:20:00Z"},
			want:       []run{{"2026-03-10T08:00:00Z", 1}, {"2026-03-10T08:10:00Z", 2}, {"2026-03-10T08:20:00Z", 1}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newRunningScheduler(t, scheduler.Config{Location: berlin(t), RetryAfter: tt.retryAfter})
			var got []run
			mustRegister(t, s, scheduler.Job{
				Name:  "sweep",
				Every: tt.every,
				Run: func(_ context.Context, r scheduler.Run) (scheduler.Outcome, error) {
					got = append(got, run{r.Due.UTC().Format(time.RFC3339), r.Attempt})
					if len(got) == 1 {
						return scheduler.Outcome{}, errors.New("boom")
					}
					return scheduler.Outcome{}, nil
				},
			})

			for _, now := range tt.ticks {
				mustTick(t, s, utc(t, now))
			}

			if !slices.Equal(got, tt.want) {
				t.Fatalf("runs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTickTakesOverAbandonedIntervalRun(t *testing.T) {
	t.Parallel()

	pool := pgtest.New(t)
	start := utc(t, "2026-03-10T08:00:00Z")
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO scheduler_intervals (job, due_at, started_at, attempt) VALUES ('sweep', $1, $2, 1)`,
		start.Add(time.Hour), start); err != nil {
		t.Fatalf("insert abandoned run: %v", err)
	}
	s := newRunningSchedulerOn(t, pool, scheduler.Config{Location: berlin(t), RetryAfter: 5 * time.Minute})
	var attempts []int
	mustRegister(t, s, scheduler.Job{
		Name:    "sweep",
		Every:   time.Hour,
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

func TestConcurrentTicksProduceOneIntervalRunPerGridPoint(t *testing.T) {
	t.Parallel()

	pool := pgtest.New(t)
	var runs atomic.Int32
	job := scheduler.Job{
		Name:  "sweep",
		Every: time.Hour,
		Run: func(context.Context, scheduler.Run) (scheduler.Outcome, error) {
			runs.Add(1)
			time.Sleep(50 * time.Millisecond)
			return scheduler.Outcome{}, nil
		},
	}
	schedulers := make([]*scheduler.Scheduler, 4)
	for i := range schedulers {
		schedulers[i] = newRunningSchedulerOn(t, pool, scheduler.Config{Location: berlin(t)}, scheduler.WithoutLeases())
		mustRegister(t, schedulers[i], job)
	}

	for _, now := range []string{"2026-03-10T08:00:00Z", "2026-03-10T08:30:00Z", "2026-03-10T09:00:00Z"} {
		var wg sync.WaitGroup
		for _, s := range schedulers {
			wg.Go(func() {
				if err := s.Tick(t.Context(), utc(t, now)); err != nil {
					t.Errorf("Tick() error = %v", err)
				}
			})
		}
		wg.Wait()
	}

	if got := runs.Load(); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
}

func TestTickReportsLastSuccessOfIntervalJob(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	s := newRunningScheduler(t,
		scheduler.Config{Location: berlin(t)},
		scheduler.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
	)
	mustRegister(t, s, scheduler.Job{
		Name:  "sweep",
		Every: time.Hour,
		Run: func(context.Context, scheduler.Run) (scheduler.Outcome, error) {
			return scheduler.Outcome{}, nil
		},
	})

	start := utc(t, "2026-03-10T08:00:00Z")
	mustTick(t, s, start)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	if got := float64Gauge(t, rm, "scheduler.job.last_success"); got < float64(start.Unix()) {
		t.Errorf("scheduler.job.last_success = %v, want >= %d", got, start.Unix())
	}
}

func TestNewLeaderReportsLastSuccessOfIntervalJobAfterFailedRun(t *testing.T) {
	t.Parallel()

	pool := pgtest.New(t)
	success := utc(t, "2026-03-10T08:00:00Z")
	previous := newRunningSchedulerOn(t, pool, scheduler.Config{Location: berlin(t)})
	mustRegister(t, previous, scheduler.Job{
		Name:  "sweep",
		Every: time.Hour,
		Run: func(_ context.Context, r scheduler.Run) (scheduler.Outcome, error) {
			if r.Due.Equal(success) {
				return scheduler.Outcome{}, nil
			}
			return scheduler.Outcome{}, errors.New("boom")
		},
	})
	mustTick(t, previous, success)
	mustTick(t, previous, success.Add(time.Hour))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := previous.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	reader := sdkmetric.NewManualReader()
	leader := newRunningSchedulerOn(t, pool,
		scheduler.Config{Location: berlin(t)},
		scheduler.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
	)
	mustRegister(t, leader, scheduler.Job{
		Name:  "sweep",
		Every: time.Hour,
		Run: func(context.Context, scheduler.Run) (scheduler.Outcome, error) {
			return scheduler.Outcome{}, errors.New("boom")
		},
	})
	mustTick(t, leader, success.Add(time.Hour+time.Minute))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	if got, want := float64Gauge(t, rm, "scheduler.job.last_success"), float64(success.Unix()); got != want {
		t.Errorf("scheduler.job.last_success = %v, want %v", got, want)
	}
}
