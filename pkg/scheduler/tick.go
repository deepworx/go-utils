package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/internal/backoff"
	"github.com/deepworx/go-utils/internal/telemetry"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
)

// statementTimeout bounds the writes that record a run's end, which run
// even after the run's context ended.
const statementTimeout = 5 * time.Second

// ErrPanic wraps the value of a panic that aborted a run.
var ErrPanic = errors.New("scheduler: run panicked")

// Tick runs every registered job that is due at now and waits for those
// runs to finish. A daily job is due once now has reached local midnight
// plus NotBefore on now's Business date and that date has no claimed run;
// earlier dates are never considered. Afterwards the leader reports the
// last_success gauge. Aborted runs are logged, not returned; Tick returns
// database and lease errors.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) error {
	jobs := s.registered()
	errs := make([]error, len(jobs)+1)
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Go(func() { errs[i] = s.tickJob(ctx, job, now) })
	}
	wg.Wait()
	errs[len(jobs)] = s.lead(ctx, jobs)
	return errors.Join(errs...)
}

// Run ticks every PollInterval until ctx ends, then releases the leader
// lease and returns nil. Each job ticks on its own, so a long run never
// delays another job. Jobs must be registered before Run is called.
func (s *Scheduler) Run(ctx context.Context) error {
	defer s.resign()
	jobs := s.registered()
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Go(func() {
			s.loop(ctx, job.Name, func() error { return s.tickJob(ctx, job, time.Now()) })
		})
	}
	s.loop(ctx, leaderName, func() error { return s.lead(ctx, jobs) })
	wg.Wait()
	return nil
}

func (s *Scheduler) loop(ctx context.Context, name string, tick func() error) {
	for {
		if err := tick(); err != nil && ctx.Err() == nil {
			s.logger.WarnContext(ctx, "scheduler: tick", slog.String(telemetry.LogJob, name), slog.Any(telemetry.LogError, err))
		}
		if !sleep(ctx, backoff.Poll(s.pollInterval, nil)) {
			return
		}
	}
}

func (s *Scheduler) tickJob(ctx context.Context, job Job, now time.Time) error {
	date := DateOf(now.In(s.location))
	due := date.midnight(s.location).Add(job.NotBefore)
	if now.Before(due) {
		return nil
	}

	leaseCtx, release, err := s.locker.tryAcquire(ctx, jobKey(s.namespace, job.Name))
	if errors.Is(err, lease.ErrHeld) || errors.Is(err, lease.ErrNotConnected) {
		s.logger.DebugContext(ctx, "scheduler: job lease unavailable", slog.String(telemetry.LogJob, job.Name), slog.Any(telemetry.LogError, err))
		return nil
	}
	if err != nil {
		return fmt.Errorf("scheduler: lease %s: %w", job.Name, err)
	}
	defer release()

	attempt, claimed, err := s.claim(leaseCtx, claimInput{job: job, date: date, now: now})
	if err != nil || !claimed {
		return err
	}
	return s.execute(leaseCtx, job, Run{Date: date, Due: due, Location: s.location, Attempt: attempt}, now)
}

type claimInput struct {
	job  Job
	date Date
	now  time.Time
}

// claim inserts the run row for the date, or takes over an aborted attempt
// after RetryAfter, or an abandoned one after Timeout plus RetryAfter. It
// reports false when the date is finished or another attempt is running.
// The upsert is the fence: at most one caller claims each attempt.
func (s *Scheduler) claim(ctx context.Context, in claimInput) (attempt int, claimed bool, err error) {
	retryBefore := in.now.Add(-s.retryAfter)
	abandonedBefore := retryBefore.Add(-in.job.Timeout)
	err = s.pool.QueryRow(ctx, `
		INSERT INTO `+s.runsTable+` AS r (job, business_date, started_at, attempt)
		VALUES ($1, $2::date, $3, 1)
		ON CONFLICT (job, business_date) DO UPDATE
		SET started_at = EXCLUDED.started_at, finished_at = NULL,
		    attempt = r.attempt + 1, failures = 0, error = NULL
		WHERE (r.error IS NOT NULL AND r.finished_at <= $4)
		   OR (r.finished_at IS NULL AND r.started_at <= $5)
		RETURNING attempt`,
		in.job.Name, in.date.String(), in.now, retryBefore, abandonedBefore,
	).Scan(&attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("scheduler: claim %s on %s: %w", in.job.Name, in.date, err)
	}
	return attempt, true, nil
}

func (s *Scheduler) execute(ctx context.Context, job Job, run Run, now time.Time) error {
	logger := s.logger.With(slog.String(telemetry.LogJob, job.Name), slog.Int(telemetry.LogAttempt, run.Attempt))
	ctx, span := s.tracer.Start(ctx, telemetry.SpanSchedulerRun(job.Name),
		trace.WithNewRoot(),
		trace.WithAttributes(telemetry.AttrJob.String(job.Name), telemetry.AttrAttempt.Int(run.Attempt)),
	)
	defer span.End()
	ctx = runContext(ctx, job.Name)

	start := time.Now()
	outcome, runErr := invoke(ctx, job, run)
	elapsed := time.Since(start)

	jobAttr := metric.WithAttributes(telemetry.AttrJob.String(job.Name))
	s.metrics.duration.Record(ctx, elapsed.Seconds(), jobAttr)
	if n := len(outcome.Failures); n > 0 {
		s.metrics.candidateFailures.Add(ctx, int64(n), jobAttr)
		for _, f := range outcome.Failures {
			logger.WarnContext(ctx, "scheduler: candidate failed", slog.String("candidate", f.Candidate), slog.Any(telemetry.LogError, f.Err))
		}
	}
	if runErr != nil {
		span.RecordError(runErr)
		span.SetStatus(codes.Error, runErr.Error())
		span.SetAttributes(telemetry.AttrErrorType.String(errorType(runErr)))
		logger.ErrorContext(ctx, "scheduler: run aborted", slog.Any(telemetry.LogError, runErr))
	}

	return s.finish(ctx, finishInput{
		job:        job.Name,
		run:        run,
		finishedAt: now.Add(elapsed),
		failures:   len(outcome.Failures),
		err:        runErr,
	})
}

// invoke calls job.Run under its Timeout and turns a panic or a timeout
// into an error.
func invoke(ctx context.Context, job Job, run Run) (outcome Outcome, err error) {
	ctx, cancel := context.WithTimeout(ctx, job.Timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrPanic, r)
		}
	}()
	outcome, err = job.Run(ctx, run)
	if err == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("scheduler: run exceeded timeout %s: %w", job.Timeout, ctx.Err())
	}
	return outcome, err
}

// runContext gives a run its own identity: the scheduler Actor, a fresh
// Correlation and no Tenant or user Claims.
func runContext(ctx context.Context, job string) context.Context {
	correlation, err := uuid.NewV7()
	if err != nil {
		correlation = uuid.New()
	}
	ctx = ctxutil.WithClaims(ctx, ctxutil.Claims{})
	ctx = ctxutil.WithTenantID(ctx, "")
	ctx = ctxutil.WithActor(ctx, "system:scheduler:"+job)
	return ctxutil.WithCorrelationID(ctx, correlation.String())
}

func errorType(err error) string {
	switch {
	case errors.Is(err, ErrPanic):
		return "panic"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return fmt.Sprintf("%T", err)
	}
}

type finishInput struct {
	job        string
	run        Run
	finishedAt time.Time
	failures   int
	err        error
}

// finish records the end of the claimed attempt. It runs even when ctx has
// ended, and is fenced by the attempt number.
func (s *Scheduler) finish(ctx context.Context, in finishInput) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statementTimeout)
	defer cancel()

	var runErr *string
	if in.err != nil {
		msg := in.err.Error()
		runErr = &msg
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE `+s.runsTable+`
		SET finished_at = $3, failures = $4, error = $5
		WHERE job = $1 AND business_date = $2::date AND attempt = $6 AND finished_at IS NULL`,
		in.job, in.run.Date.String(), in.finishedAt, in.failures, runErr, in.run.Attempt,
	)
	if err != nil {
		return fmt.Errorf("scheduler: finish %s on %s: %w", in.job, in.run.Date, err)
	}
	if tag.RowsAffected() == 0 {
		s.logger.WarnContext(ctx, "scheduler: run was claimed by another attempt",
			slog.String(telemetry.LogJob, in.job), slog.Int(telemetry.LogAttempt, in.run.Attempt))
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
