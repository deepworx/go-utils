package reactor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/deepworx/go-utils/internal/backoff"
	"github.com/deepworx/go-utils/internal/consumerhook"
	"github.com/deepworx/go-utils/internal/telemetry"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// StartAt is where a Reactor begins when its Cursor does not exist yet.
type StartAt int

const (
	// FromNow seeds the Cursor at the head of the log on the first claim,
	// so the Reactor never reacts to history.
	FromNow StartAt = iota + 1
	// FromBeginning reacts to the whole log.
	FromBeginning
)

// Config configures a Reactor.
type Config struct {
	// Name identifies the Reactor, its Cursor and its parked events.
	Name string
	// Handler reacts to an event, usually by issuing commands through the
	// app's Repository. A Router also provides the filter.
	Handler consumer.Handler
	// EventTypes is the filter; see consumer.Config.EventTypes.
	EventTypes []string
	// StartAt is required.
	StartAt StartAt
}

// Option configures a Reactor.
type Option func(*options)

type options struct {
	retry   consumer.Retry
	timeout time.Duration
	logger  *slog.Logger
}

// WithRetry sets how often a failing reaction is tried before its Stream is
// Parked. It also retries a failed write of the park list before the
// Reactor goes Stopped. The default is 5 attempts with delays from 1s up to
// 30s.
func WithRetry(r consumer.Retry) Option {
	return func(o *options) { o.retry = r }
}

// WithTimeout bounds one reaction attempt. A reaction still running at the
// timeout has its ctx cancelled and counts as failed. The default is 60s.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

func newOptions(opts []Option) options {
	o := options{
		retry:   consumer.Retry{MaxAttempts: 5, Base: time.Second, Max: 30 * time.Second},
		timeout: 60 * time.Second,
		logger:  slog.Default(),
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// Reactor is a stateless Consumer that turns events into commands. It
// always runs in consumer.OwnTransaction mode, so delivery is at least once:
// env.EventID is the idempotency key for outbound calls, and the library
// does not deduplicate.
//
// A reaction that keeps failing, or fails with consumer.Permanent, a panic
// or an undecodable event, Parks its Stream: the event and every later event
// of the Stream are held in order while the Cursor moves on for all other
// Streams. Only a failure to write the park list makes the Reactor retry
// and finally go Stopped.
type Reactor struct {
	name     string
	actor    string
	handler  consumer.Handler
	opts     options
	retry    backoff.Retry
	parks    parkStore
	consumer *consumer.Consumer
	logger   *slog.Logger
}

const actorPrefix = "system:reactor:"

// New creates a Reactor on rt. It fails with ErrStartAtRequired when
// StartAt is unset, and otherwise like consumer.New.
func New(rt consumer.Runtime, cfg Config, opts ...Option) (*Reactor, error) {
	if cfg.StartAt == 0 {
		return nil, fmt.Errorf("new reactor %q: %w", cfg.Name, ErrStartAtRequired)
	}
	if cfg.StartAt != FromNow && cfg.StartAt != FromBeginning {
		return nil, fmt.Errorf("new reactor %q: start at %d: %w", cfg.Name, cfg.StartAt, consumer.ErrInvalidConfig)
	}
	o := newOptions(opts)
	if o.timeout <= 0 || o.logger == nil {
		return nil, fmt.Errorf("new reactor %q: timeout %s, logger set %t: %w",
			cfg.Name, o.timeout, o.logger != nil, consumer.ErrInvalidConfig)
	}
	backend, ok := consumerhook.RuntimeBackend(rt)
	if !ok {
		return nil, fmt.Errorf("new reactor %q: unknown runtime: %w", cfg.Name, consumer.ErrInvalidConfig)
	}

	r := &Reactor{
		name:    cfg.Name,
		actor:   actorPrefix + cfg.Name,
		handler: cfg.Handler,
		opts:    o,
		retry:   backoff.Retry(o.retry),
		logger:  o.logger.With(telemetry.LogConsumer, cfg.Name),
	}
	r.parks = newParkStore(backend, cfg.Name)

	hook, _ := consumerhook.ReactorOption(consumerhook.ReactorOptions{
		SeedAtHead:   cfg.StartAt == FromNow,
		DecodeFailed: r.decodeFailed,
	}).(consumer.Option)
	var handler consumer.Handler
	if cfg.Handler != nil {
		handler = consumer.HandlerFunc(r.handle)
	}
	c, err := consumer.New(rt, consumer.Config{
		Name:       cfg.Name,
		EventTypes: filter(cfg),
		Mode:       consumer.OwnTransaction,
		Handler:    handler,
	}, consumer.WithRetry(o.retry), consumer.WithLogger(o.logger), consumer.WithEventSpans(true), hook)
	if err != nil {
		return nil, fmt.Errorf("new reactor: %w", err)
	}
	r.consumer = c
	return r, nil
}

// filter is the Consumer filter: Config.EventTypes, else the Handler's.
func filter(cfg Config) []string {
	if len(cfg.EventTypes) > 0 {
		return cfg.EventTypes
	}
	if typer, ok := cfg.Handler.(interface{ EventTypes() []string }); ok {
		return slices.Clone(typer.EventTypes())
	}
	return nil
}

// Consumer returns the Reactor's Consumer, to run in a consumer.Group.
func (r *Reactor) Consumer() *consumer.Consumer { return r.consumer }

// handle is the Consumer's handler: it holds env behind a Parked Stream,
// or reacts and Parks the Stream when the reaction fails for good.
func (r *Reactor) handle(ctx context.Context, env eventstore.Envelope) error {
	held, err := r.parks.holdIfParked(ctx, parkInput{env: env, at: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("check park list: %w", err)
	}
	if held {
		return nil
	}
	attempts, err := r.reactWithRetry(ctx, env)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return r.park(ctx, parkInput{env: env, cause: err.Error(), attempts: attempts})
}

// decodeFailed Parks an undecodable event at once.
func (r *Reactor) decodeFailed(ctx context.Context, env eventstore.Envelope, payload eventstore.Payload, cause error) error {
	in := parkInput{env: env, payload: &payload, at: time.Now().UTC()}
	held, err := r.parks.holdIfParked(ctx, in)
	if err != nil {
		return fmt.Errorf("check park list: %w", err)
	}
	if held {
		return nil
	}
	in.cause, in.attempts = cause.Error(), 1
	return r.park(ctx, in)
}

func (r *Reactor) park(ctx context.Context, in parkInput) error {
	in.at = time.Now().UTC()
	if err := r.parks.park(ctx, in); err != nil {
		return fmt.Errorf("park event %s: %w", in.env.EventID, err)
	}
	r.logger.WarnContext(ctx, "reactor parked stream",
		append(eventAttrs(in.env), telemetry.LogAttempt, in.attempts, telemetry.LogError, in.cause)...)
	return nil
}

// reactWithRetry reacts to env until it succeeds, fails Permanently, the
// retries are spent or ctx ends. attempts counts the reactions run.
func (r *Reactor) reactWithRetry(ctx context.Context, env eventstore.Envelope) (attempts int, err error) {
	for attempts = 1; ; attempts++ {
		if err = r.react(ctx, env); err == nil {
			return attempts, nil
		}
		delay, ok := r.retry.Delay(attempts, nil)
		if !ok || consumerhook.IsPermanent(err) || ctx.Err() != nil {
			return attempts, err
		}
		r.logger.WarnContext(ctx, "reaction failed, retrying",
			append(eventAttrs(env), telemetry.LogAttempt, attempts, telemetry.LogError, err)...)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return attempts, context.Cause(ctx)
		case <-timer.C:
		}
	}
}

// react runs one reaction attempt under the timeout, with the Reactor's
// Actor, env as Causation and env's Tenant and Correlation in ctx. A panic
// is Permanent; a reaction outliving its timeout fails.
func (r *Reactor) react(ctx context.Context, env eventstore.Envelope) (err error) {
	ctx = ctxutil.WithTenantID(ctx, env.TenantID)
	ctx = ctxutil.WithCorrelationID(ctx, env.CorrelationID)
	ctx = ctxutil.WithActor(ctx, r.actor)
	ctx = ctxutil.WithCausationID(ctx, env.EventID)
	ctx, cancel := context.WithTimeout(ctx, r.opts.timeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			err = consumer.Permanent(fmt.Errorf("reaction panic: %v", p))
		}
	}()
	err = r.handler.Handle(ctx, env)
	if err == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("reaction exceeded timeout %s: %w", r.opts.timeout, ctx.Err())
	}
	return err
}

func eventAttrs(env eventstore.Envelope) []any {
	return []any{
		telemetry.LogTenantID, env.TenantID,
		telemetry.LogStreamType, env.StreamType,
		telemetry.LogStreamID, env.StreamID,
		telemetry.LogEventID, env.EventID,
		telemetry.LogEventType, env.EventType,
	}
}
