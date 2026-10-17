// Package scheduler runs jobs that are due at most once and never catches
// up on missed runs (ADR-0008). It is Postgres-only.
//
// A daily job runs at most once per Business date in Config.Location. It is
// due from local midnight plus NotBefore; missed dates are never run
// retroactively. Each run claims one scheduler_runs row per (job,
// business_date), and that claim upsert is the fence against duplicate
// runs.
//
// An interval job runs on a fixed grid of multiples of Every on Unix time,
// so rollouts and restarts never shift it. Its scheduler_intervals row holds
// the next due_at, claimed with a conditional upsert; after an outage the
// job runs once and due_at jumps to the next grid point after now.
//
// Leases (a leader lease plus one per job, under the namespace
// "<schema>.scheduler") only prevent duplicate work.
//
// Tick(ctx, now) is the synchronous seam that runs everything due at now;
// Run(ctx) calls it in a loop. Jobs must be idempotent for the same date or
// grid point, since an aborted run (error, timeout or panic) is retried
// after Config.RetryAfter, or for interval jobs after min(RetryAfter,
// Every).
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/internal/telemetry"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
)

const scope = "github.com/deepworx/go-utils/pkg/scheduler"

const (
	defaultSchema       = "public"
	defaultRetryAfter   = 5 * time.Minute
	defaultTimeout      = 30 * time.Minute
	defaultPollInterval = 30 * time.Second
)

var (
	// ErrNoLocation is returned by New when Config.Location is nil. There
	// is no UTC default: the business timezone must be chosen explicitly.
	ErrNoLocation = errors.New("scheduler: no location")
	// ErrDuplicate is returned by Register for a job name that is already
	// registered.
	ErrDuplicate = errors.New("scheduler: duplicate job")
	// ErrInvalidJob is returned by Register for a job without a name or Run
	// func, with a negative duration, with NotBefore of a day or more, or
	// with NotBefore on an interval job.
	ErrInvalidJob = errors.New("scheduler: invalid job")
)

// Config configures a Scheduler.
type Config struct {
	// Schema holds the scheduler tables. Defaults to "public".
	Schema string
	// Location is the business timezone that defines a Business date.
	// Required.
	Location *time.Location
	// RetryAfter is how long an aborted run waits before it is retried; an
	// interval job waits at most Every. Defaults to 5m.
	RetryAfter time.Duration
	// PollInterval is how often Run ticks. Defaults to 30s.
	PollInterval time.Duration
}

// Option configures a Scheduler beyond Config.
type Option func(*options)

type options struct {
	providers telemetry.Providers
	logger    *slog.Logger
	locker    locker
}

// WithLogger logs with logger instead of slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// WithMeterProvider records metrics with mp instead of the global provider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.providers.MeterProvider = mp }
}

// WithTracerProvider records spans with tp instead of the global provider.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.providers.TracerProvider = tp }
}

// Job is a scheduled unit of work.
type Job struct {
	// Name identifies the job in the scheduler tables, leases and telemetry.
	Name string
	// Every is the interval of an interval job, whose runs are due at the
	// multiples of Every on Unix time; 0 makes a daily job.
	Every time.Duration
	// NotBefore delays a daily job past local midnight. It is added to the
	// midnight instant, so a run due inside a DST gap still runs. It must be
	// less than a day, and zero for interval jobs.
	NotBefore time.Duration
	// Timeout bounds one run. Defaults to 30m.
	Timeout time.Duration
	// Run does the work. An error, a timeout or a panic aborts the run,
	// which is then retried; per-candidate failures belong in Outcome.
	Run func(ctx context.Context, run Run) (Outcome, error)
}

// Run describes one run of a job.
type Run struct {
	// Date is the Business date the run is for. For an interval job it is
	// the Business date of Due.
	Date Date
	// Due is the instant the run became due. For an interval job it is the
	// grid point the run is for.
	Due time.Time
	// Location is the business timezone.
	Location *time.Location
	// Attempt counts the attempts for Date, starting at 1. For an interval
	// job it counts the attempts since the last successful run.
	Attempt int
}

// Outcome reports what a finished run did.
type Outcome struct {
	// Candidates is the number of candidates the run considered.
	Candidates int
	// Failures lists candidates that failed without aborting the run.
	Failures []Failure
}

// Failure is one failed candidate of a run.
type Failure struct {
	// Candidate identifies the failed candidate, such as a stream ID.
	Candidate string
	// Err is why it failed.
	Err error
}

// Scheduler runs registered jobs when they are due.
type Scheduler struct {
	pool           *pgxpool.Pool
	locker         locker
	location       *time.Location
	retryAfter     time.Duration
	pollInterval   time.Duration
	namespace      string
	runsTable      string
	intervalsTable string
	logger         *slog.Logger
	tracer         trace.Tracer
	metrics        metrics

	mu     sync.Mutex
	jobs   []Job
	leader leader
}

// New returns a Scheduler storing its runs through pool and taking its
// leases from claims, whose Run must be running.
func New(pool *pgxpool.Pool, claims *lease.Claims, cfg Config, opts ...Option) (*Scheduler, error) {
	if cfg.Location == nil {
		return nil, ErrNoLocation
	}
	o := options{logger: slog.Default(), locker: claimsLocker{claims}}
	for _, opt := range opts {
		opt(&o)
	}
	if cfg.Schema == "" {
		cfg.Schema = defaultSchema
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = defaultRetryAfter
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	m, err := newMetrics(o.providers.Meter(scope))
	if err != nil {
		o.logger.Warn("scheduler: create metrics", slog.Any(telemetry.LogError, err))
	}
	return &Scheduler{
		pool:           pool,
		locker:         o.locker,
		location:       cfg.Location,
		retryAfter:     cfg.RetryAfter,
		pollInterval:   cfg.PollInterval,
		namespace:      cfg.Schema + ".scheduler",
		runsTable:      pgx.Identifier{cfg.Schema, "scheduler_runs"}.Sanitize(),
		intervalsTable: pgx.Identifier{cfg.Schema, "scheduler_intervals"}.Sanitize(),
		logger:         o.logger,
		tracer:         o.providers.Tracer(scope),
		metrics:        m,
	}, nil
}

// Register adds job. It returns ErrDuplicate for a name that is already
// registered and ErrInvalidJob for an invalid job. Jobs registered while
// Run is running are picked up when Run starts again.
func (s *Scheduler) Register(job Job) error {
	if err := validate(job); err != nil {
		return err
	}
	if job.Timeout == 0 {
		job.Timeout = defaultTimeout
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.Name == job.Name {
			return fmt.Errorf("%w: %s", ErrDuplicate, job.Name)
		}
	}
	s.jobs = append(s.jobs, job)
	return nil
}

func validate(job Job) error {
	switch {
	case job.Name == "":
		return fmt.Errorf("%w: empty name", ErrInvalidJob)
	case job.Run == nil:
		return fmt.Errorf("%w: %s has no Run func", ErrInvalidJob, job.Name)
	case job.Every < 0:
		return fmt.Errorf("%w: %s: negative Every", ErrInvalidJob, job.Name)
	case job.Every > 0 && job.NotBefore != 0:
		return fmt.Errorf("%w: %s: NotBefore is only for daily jobs", ErrInvalidJob, job.Name)
	case job.NotBefore < 0 || job.NotBefore >= 24*time.Hour:
		return fmt.Errorf("%w: %s: NotBefore must be in [0, 24h)", ErrInvalidJob, job.Name)
	case job.Timeout < 0:
		return fmt.Errorf("%w: %s: negative Timeout", ErrInvalidJob, job.Name)
	}
	return nil
}

func (s *Scheduler) registered() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Job(nil), s.jobs...)
}
