package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/deepworx/go-utils/internal/telemetry"
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

// leader is the held leader lease, if any. done closes when it is lost.
type leader struct {
	done    <-chan struct{}
	release func()
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

// lead keeps or takes the leader lease and, while held, reports the
// last_success gauge of jobs. The lease outlives the call; resign releases
// it.
func (s *Scheduler) lead(ctx context.Context, jobs []Job) error {
	held, err := s.ensureLeader(ctx)
	if err != nil || !held {
		return err
	}
	return s.reportLastSuccess(ctx, jobs)
}

func (s *Scheduler) ensureLeader(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leader.held() {
		return true, nil
	}
	if s.leader.release != nil {
		s.leader.release()
		s.leader = leader{}
	}

	leaseCtx, release, err := s.locker.tryAcquire(context.WithoutCancel(ctx), lease.Key{Namespace: s.namespace, Name: leaderName})
	if errors.Is(err, lease.ErrHeld) || errors.Is(err, lease.ErrNotConnected) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("scheduler: leader lease: %w", err)
	}
	s.leader = leader{done: leaseCtx.Done(), release: release}
	return true, nil
}

// resign releases the leader lease.
func (s *Scheduler) resign() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leader.release != nil {
		s.leader.release()
	}
	s.leader = leader{}
}

func (s *Scheduler) reportLastSuccess(ctx context.Context, jobs []Job) error {
	names := make([]string, len(jobs))
	for i, job := range jobs {
		names[i] = job.Name
	}
	rows, err := s.pool.Query(ctx, `
		SELECT job, max(finished_at)
		FROM (
			SELECT job, finished_at, error FROM `+s.runsTable+`
			UNION ALL
			SELECT job, finished_at, error FROM `+s.intervalsTable+`
		) runs
		WHERE job = ANY($1) AND finished_at IS NOT NULL AND error IS NULL
		GROUP BY job`, names)
	if err != nil {
		return fmt.Errorf("scheduler: query last success: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			job      string
			finished time.Time
		)
		if err := rows.Scan(&job, &finished); err != nil {
			return fmt.Errorf("scheduler: scan last success: %w", err)
		}
		s.metrics.lastSuccess.Record(ctx, float64(finished.Unix()), metric.WithAttributes(telemetry.AttrJob.String(job)))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scheduler: read last success: %w", err)
	}
	return nil
}
