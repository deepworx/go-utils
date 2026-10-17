package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/deepworx/go-utils/pkg/postgres/lease"
)

const leaderName = "leader"

// locker takes leases without waiting. release is idempotent.
type locker interface {
	tryAcquire(ctx context.Context, key lease.Key) (leaseCtx context.Context, release func(), err error)
}

type claimsLocker struct{ claims *lease.Claims }

func (c claimsLocker) tryAcquire(ctx context.Context, key lease.Key) (context.Context, func(), error) {
	leaseCtx, l, err := c.claims.TryAcquire(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	return leaseCtx, l.Release, nil
}

// leader is the held leader lease, if any. done closes when it is lost;
// stop releases it and ends its gauge reporting.
type leader struct {
	done <-chan struct{}
	stop func()
}

func (l leader) held() bool {
	if l.done == nil {
		return false
	}
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

func jobKey(namespace, job string) lease.Key {
	return lease.Key{Namespace: namespace, Name: "job:" + job}
}

// lead keeps or takes the leader lease. While it is held, the
// last_success gauge of jobs is reported. The lease outlives the call;
// resign releases it.
func (s *Scheduler) lead(ctx context.Context, jobs []Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leader.held() {
		return nil
	}
	if s.leader.stop != nil {
		s.leader.stop()
		s.leader = leader{}
	}

	leaseCtx, release, err := s.locker.tryAcquire(context.WithoutCancel(ctx), lease.Key{Namespace: s.namespace, Name: leaderName})
	if errors.Is(err, lease.ErrHeld) || errors.Is(err, lease.ErrNotConnected) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scheduler: leader lease: %w", err)
	}
	names := make([]string, len(jobs))
	for i, job := range jobs {
		names[i] = job.Name
	}
	done := leaseCtx.Done()
	stopGauge := s.metrics.observe(observeInput{
		jobs: names,
		lastSuccess: func(ctx context.Context, jobs []string) (map[string]time.Time, error) {
			select {
			case <-done:
				return nil, nil
			default:
				return s.lastSuccess(ctx, jobs)
			}
		},
	})
	s.leader = leader{done: done, stop: func() {
		stopGauge()
		release()
	}}
	return nil
}

// resign releases the leader lease.
func (s *Scheduler) resign() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leader.stop != nil {
		s.leader.stop()
	}
	s.leader = leader{}
}

// lastSuccess returns when each of jobs last finished without error; jobs
// without such a run are absent.
func (s *Scheduler) lastSuccess(ctx context.Context, jobs []string) (map[string]time.Time, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT job, max(finished_at)
		FROM (
			SELECT job, finished_at, error FROM `+s.runsTable+`
			UNION ALL
			SELECT job, finished_at, error FROM `+s.intervalsTable+`
		) runs
		WHERE job = ANY($1) AND finished_at IS NOT NULL AND error IS NULL
		GROUP BY job`, jobs)
	if err != nil {
		return nil, fmt.Errorf("scheduler: query last success: %w", err)
	}
	finished := make(map[string]time.Time, len(jobs))
	var (
		job string
		at  time.Time
	)
	_, err = pgx.ForEachRow(rows, []any{&job, &at}, func() error {
		finished[job] = at
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scheduler: read last success: %w", err)
	}
	return finished, nil
}
