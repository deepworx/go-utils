package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// gridPoint returns the latest multiple of every on Unix time at or before
// t.
func gridPoint(t time.Time, every time.Duration) time.Time {
	n := t.UnixNano()
	offset := n % int64(every)
	if offset < 0 {
		offset += int64(every)
	}
	return time.Unix(0, n-offset).UTC()
}

func (s *Scheduler) intervalRetryAfter(job Job) time.Duration {
	return min(s.retryAfter, job.Every)
}

// claimInterval runs the job for the latest grid point when its row's
// due_at has passed, or takes over an abandoned attempt after Timeout plus
// the retry delay. A claim moves due_at to the next grid point after now,
// so missed grid points are skipped. The upsert is the fence: at most one
// caller claims each attempt.
func (s *Scheduler) claimInterval(ctx context.Context, job Job, now time.Time) (Run, bool, error) {
	due := gridPoint(now, job.Every)
	abandonedBefore := now.Add(-job.Timeout - s.intervalRetryAfter(job))
	var attempt int
	err := s.pool.QueryRow(ctx, `
		INSERT INTO `+s.intervalsTable+` AS r (job, due_at, started_at, attempt)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (job) DO UPDATE
		SET due_at = EXCLUDED.due_at, started_at = EXCLUDED.started_at, finished_at = NULL,
		    attempt = CASE WHEN r.finished_at IS NOT NULL AND r.error IS NULL THEN 1 ELSE r.attempt + 1 END,
		    failures = 0, error = NULL
		WHERE (r.finished_at IS NOT NULL AND r.due_at <= $3)
		   OR (r.finished_at IS NULL AND r.started_at <= $4)
		RETURNING attempt`,
		job.Name, due.Add(job.Every), now, abandonedBefore,
	).Scan(&attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("scheduler: claim %s at %s: %w", job.Name, due.Format(time.RFC3339), err)
	}
	return Run{Date: DateOf(due.In(s.location)), Due: due, Location: s.location, Attempt: attempt}, true, nil
}

// finishInterval records the end of the claimed attempt, fenced by its
// attempt number and start. An aborted attempt becomes due again after the
// retry delay, or at the next grid point if that comes first. A successful
// attempt sets last_success_at, which failed attempts keep.
func (s *Scheduler) finishInterval(ctx context.Context, in finishInput) error {
	var retryAt, succeededAt *time.Time
	if in.err != nil {
		at := in.finishedAt.Add(s.intervalRetryAfter(in.job))
		retryAt = &at
	} else {
		succeededAt = &in.finishedAt
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE `+s.intervalsTable+`
		SET finished_at = $4, failures = $5, error = $6, due_at = LEAST(due_at, $7::timestamptz),
		    last_success_at = COALESCE($8::timestamptz, last_success_at)
		WHERE job = $1 AND attempt = $2 AND started_at = $3 AND finished_at IS NULL`,
		in.job.Name, in.run.Attempt, in.startedAt, in.finishedAt, in.failures, errorText(in.err), retryAt, succeededAt,
	)
	if err != nil {
		return fmt.Errorf("scheduler: finish %s at %s: %w", in.job.Name, in.run.Due.Format(time.RFC3339), err)
	}
	if tag.RowsAffected() == 0 {
		s.warnSuperseded(ctx, in)
	}
	return nil
}
