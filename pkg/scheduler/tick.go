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
	"github.com/deepworx/go-utils/internal/safecall"
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
// earlier dates are never considered. An interval job is due once now has
// reached its next grid point; it then runs once for the latest grid point,
// however many were missed. Afterwards Tick keeps or takes the leader
// lease, whose holder reports the last_success gauge. Aborted runs are
// logged, not returned; Tick returns database and lease errors.
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
			s.loop(ctx, job.Name, func() error { return s.tickJob(ctx, job, s.now()) })
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
	if job.Every == 0 && now.Before(s.dailyDue(job, now)) {
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

	claim := s.claimDaily
	if job.Every > 0 {
		claim = s.claimInterval
	}
	run, claimed, err := claim(leaseCtx, job, now)
	if err != nil || !claimed {
		return err
	}
	return s.execute(leaseCtx, executeInput{job: job, run: run, startedAt: now})
}

// dailyDue returns the instant a daily job becomes due on now's Business
// date: local midnight plus NotBefore. On a day shorter than that, such as
// the 23h spring-forward day, it is moved back to the same distance before
// the next midnight, so the date still runs.
func (s *Scheduler) dailyDue(job Job, now time.Time) time.Time {
	date := DateOf(now.In(s.location))
	due := date.midnight(s.location).Add(job.NotBefore)
	next := date.next().midnight(s.location)
	if due.Before(next) {
		return due
	}
	return next.Add(job.NotBefore - 24*time.Hour)
}

// claimDaily inserts the run row for now's Business date, or takes over an
// aborted attempt after RetryAfter, or an abandoned one after Timeout plus
// RetryAfter. It reports false when the date is finished or another attempt
// is running. The upsert is the fence: at most one caller claims each
// attempt.
func (s *Scheduler) claimDaily(ctx context.Context, job Job, now time.Time) (Run, bool, error) {
	date := DateOf(now.In(s.location))
	retryBefore := now.Add(-s.retryAfter)
	abandonedBefore := retryBefore.Add(-job.Timeout)
	var attempt int
	err := s.pool.QueryRow(ctx, `
		INSERT INTO `+s.runsTable+` AS r (job, business_date, started_at, attempt)
		VALUES ($1, $2::date, $3, 1)
		ON CONFLICT (job, business_date) DO UPDATE
		SET started_at = EXCLUDED.started_at, finished_at = NULL,
		    attempt = r.attempt + 1, failures = 0, error = NULL
		WHERE (r.error IS NOT NULL AND r.finished_at <= $4)
		   OR (r.finished_at IS NULL AND r.started_at <= $5)
		RETURNING attempt`,
		job.Name, date.String(), now, retryBefore, abandonedBefore,
	).Scan(&attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("scheduler: claim %s on %s: %w", job.Name, date, err)
	}
	return Run{Date: date, Due: s.dailyDue(job, now), Location: s.location, Attempt: attempt}, true, nil
}

// executeInput is a claimed run of job that started at startedAt.
type executeInput struct {
	job       Job
	run       Run
	startedAt time.Time
}

// execute runs a claimed run and records its end.
func (s *Scheduler) execute(ctx context.Context, in executeInput) error {
	job, run := in.job, in.run
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
			logger.WarnContext(ctx, "scheduler: candidate failed", slog.String(telemetry.LogCandidate, f.Candidate), slog.Any(telemetry.LogError, f.Err))
		}
	}
	if runErr != nil {
		span.RecordError(runErr)
		span.SetStatus(codes.Error, runErr.Error())
		span.SetAttributes(telemetry.AttrErrorType.String(errorType(runErr)))
		logger.ErrorContext(ctx, "scheduler: run aborted", slog.Any(telemetry.LogError, runErr))
	}

	return s.finish(ctx, finishInput{
		job:        job,
		run:        run,
		startedAt:  in.startedAt,
		finishedAt: in.startedAt.Add(elapsed),
		failures:   len(outcome.Failures),
		err:        runErr,
	})
}

// invoke calls job.Run under its Timeout and turns a panic or a timeout
// into an error.
func invoke(ctx context.Context, job Job, run Run) (Outcome, error) {
	var outcome Outcome
	err := safecall.Call(ctx, safecall.Input{Timeout: job.Timeout, LateSuccessFails: true},
		func(ctx context.Context) (err error) {
			outcome, err = job.Run(ctx, run)
			return err
		})
	if p := (*safecall.PanicError)(nil); errors.As(err, &p) {
		return outcome, fmt.Errorf("%w: %v", ErrPanic, p.Value)
	}
	if errors.Is(err, safecall.ErrTimeout) {
		return outcome, fmt.Errorf("scheduler: run %w", err)
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
	job        Job
	run        Run
	startedAt  time.Time
	finishedAt time.Time
	failures   int
	err        error
}

// finish records the end of the claimed attempt. It runs even when ctx has
// ended, and is fenced by the attempt.
func (s *Scheduler) finish(ctx context.Context, in finishInput) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statementTimeout)
	defer cancel()
	if in.job.Every > 0 {
		return s.finishInterval(ctx, in)
	}
	return s.finishDaily(ctx, in)
}

func (s *Scheduler) finishDaily(ctx context.Context, in finishInput) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE `+s.runsTable+`
		SET finished_at = $3, failures = $4, error = $5
		WHERE job = $1 AND business_date = $2::date AND attempt = $6 AND finished_at IS NULL`,
		in.job.Name, in.run.Date.String(), in.finishedAt, in.failures, errorText(in.err), in.run.Attempt,
	)
	if err != nil {
		return fmt.Errorf("scheduler: finish %s on %s: %w", in.job.Name, in.run.Date, err)
	}
	if tag.RowsAffected() == 0 {
		s.warnSuperseded(ctx, in)
	}
	return nil
}

func errorText(err error) *string {
	if err == nil {
		return nil
	}
	msg := err.Error()
	return &msg
}

func (s *Scheduler) warnSuperseded(ctx context.Context, in finishInput) {
	s.logger.WarnContext(ctx, "scheduler: run was claimed by another attempt",
		slog.String(telemetry.LogJob, in.job.Name), slog.Int(telemetry.LogAttempt, in.run.Attempt))
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
