package scheduler_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
	"github.com/deepworx/go-utils/pkg/scheduler"
)

func TestTickRunsDailyJobOncePerBusinessDate(t *testing.T) {
	t.Parallel()

	type run struct {
		date string
		due  time.Time
	}
	tests := []struct {
		name      string
		notBefore time.Duration
		ticks     []time.Time
		want      []run
	}{
		{
			name:  "second tick on the same day does not run again",
			ticks: []time.Time{utc(t, "2026-03-10T08:00:00Z"), utc(t, "2026-03-10T17:00:00Z")},
			want:  []run{{"2026-03-10", utc(t, "2026-03-09T23:00:00Z")}},
		},
		{
			name:      "not run before NotBefore",
			notBefore: 6 * time.Hour,
			ticks:     []time.Time{utc(t, "2026-03-10T04:59:59Z"), utc(t, "2026-03-10T05:00:00Z")},
			want:      []run{{"2026-03-10", utc(t, "2026-03-10T05:00:00Z")}},
		},
		{
			name:  "no catch-up after a multi-day gap",
			ticks: []time.Time{utc(t, "2026-03-10T08:00:00Z"), utc(t, "2026-03-14T08:00:00Z")},
			want: []run{
				{"2026-03-10", utc(t, "2026-03-09T23:00:00Z")},
				{"2026-03-14", utc(t, "2026-03-13T23:00:00Z")},
			},
		},
		{
			name: "spring forward",
			ticks: []time.Time{
				utc(t, "2026-03-28T22:30:00Z"), // 23:30 CET on 03-28
				utc(t, "2026-03-28T23:30:00Z"), // 00:30 CET on 03-29
				utc(t, "2026-03-29T01:30:00Z"), // 03:30 CEST on 03-29
				utc(t, "2026-03-29T22:30:00Z"), // 00:30 CEST on 03-30
			},
			want: []run{
				{"2026-03-28", utc(t, "2026-03-27T23:00:00Z")},
				{"2026-03-29", utc(t, "2026-03-28T23:00:00Z")},
				{"2026-03-30", utc(t, "2026-03-29T22:00:00Z")},
			},
		},
		{
			name:      "NotBefore inside the spring-forward gap",
			notBefore: 2*time.Hour + 30*time.Minute,
			ticks: []time.Time{
				utc(t, "2026-03-29T01:29:59Z"), // 03:29:59 CEST
				utc(t, "2026-03-29T01:30:00Z"), // 03:30 CEST; 02:30 wall time does not exist
			},
			want: []run{{"2026-03-29", utc(t, "2026-03-29T01:30:00Z")}},
		},
		{
			name:      "fall back",
			notBefore: 3 * time.Hour,
			ticks: []time.Time{
				utc(t, "2026-10-25T00:30:00Z"), // 02:30 CEST, first pass
				utc(t, "2026-10-25T01:00:00Z"), // 02:00 CET, second pass
				utc(t, "2026-10-25T01:30:00Z"), // 02:30 CET, second pass
				utc(t, "2026-10-25T22:59:59Z"), // 23:59:59 CET on 10-25
				utc(t, "2026-10-26T02:00:00Z"), // 03:00 CET on 10-26
			},
			want: []run{
				{"2026-10-25", utc(t, "2026-10-25T01:00:00Z")},
				{"2026-10-26", utc(t, "2026-10-26T02:00:00Z")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			loc := berlin(t)
			s := newRunningScheduler(t, scheduler.Config{Location: loc})
			var got []run
			mustRegister(t, s, scheduler.Job{
				Name:      "eod",
				NotBefore: tt.notBefore,
				Run: func(_ context.Context, r scheduler.Run) (scheduler.Outcome, error) {
					if r.Location != loc {
						t.Errorf("Run.Location = %v, want %v", r.Location, loc)
					}
					got = append(got, run{r.Date.String(), r.Due.UTC()})
					return scheduler.Outcome{}, nil
				},
			})

			for _, now := range tt.ticks {
				mustTick(t, s, now)
			}

			if !slices.Equal(got, tt.want) {
				t.Fatalf("runs = %v, want %v", got, tt.want)
			}
		})
	}
}

func utc(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

// newRunningScheduler returns a Scheduler whose Claims are running and
// connected, so that Tick can take leases.
func newRunningScheduler(t *testing.T, cfg scheduler.Config, opts ...scheduler.Option) *scheduler.Scheduler {
	t.Helper()
	return newRunningSchedulerOn(t, pgtest.New(t), cfg, opts...)
}

func newRunningSchedulerOn(t *testing.T, pool *pgxpool.Pool, cfg scheduler.Config, opts ...scheduler.Option) *scheduler.Scheduler {
	t.Helper()
	claims := lease.NewClaims(pool, lease.Config{})
	startClaims(t, claims)
	s, err := scheduler.New(pool, claims, cfg, opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return s
}

func startClaims(t *testing.T, claims *lease.Claims) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := claims.Run(ctx); err != nil {
			t.Errorf("Claims.Run() error = %v", err)
		}
	})
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	probe := lease.Key{Namespace: "test", Name: "probe"}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, l, err := claims.TryAcquire(t.Context(), probe)
		if err == nil {
			l.Release()
			return
		}
		if !errors.Is(err, lease.ErrNotConnected) || time.Now().After(deadline) {
			t.Fatalf("wait for Claims: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mustRegister(t *testing.T, s *scheduler.Scheduler, job scheduler.Job) {
	t.Helper()
	if err := s.Register(job); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
}

func mustTick(t *testing.T, s *scheduler.Scheduler, now time.Time) {
	t.Helper()
	if err := s.Tick(t.Context(), now); err != nil {
		t.Fatalf("Tick(%s) error = %v", now, err)
	}
}
