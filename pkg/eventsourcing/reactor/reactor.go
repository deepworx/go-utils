package reactor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/metric"

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
	retry         consumer.Retry
	timeout       time.Duration
	redrive       backoff.Steps
	logger        *slog.Logger
	now           func() time.Time
	meterProvider metric.MeterProvider
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

// WithRedrive sets the auto-redrive schedule of a Parked Stream: the
// delay before each redrive, counted from the park or the previous failed
// redrive and jittered down by up to a fifth. After the last step the hold
// is exhausted and only a manual Redrive or Discard releases it. No steps
// disable auto-redrive. The default is 1m, 5m, 30m, 2h, 12h.
func WithRedrive(steps ...time.Duration) Option {
	return func(o *options) { o.redrive = slices.Clone(steps) }
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// WithClock sets the clock that stamps parks and discards and decides
// which redrives are due. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// WithMeterProvider sets the meter provider of the Reactor's and its
// Consumer's instruments. The default is the OTel global.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProvider = mp }
}

func newOptions(opts []Option) options {
	o := options{
		retry:   consumer.Retry{MaxAttempts: 5, Base: time.Second, Max: 30 * time.Second},
		timeout: 60 * time.Second,
		redrive: backoff.Steps{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour},
		logger:  slog.Default(),
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func (o options) validate() error {
	switch {
	case o.timeout <= 0:
		return fmt.Errorf("timeout %s: %w", o.timeout, consumer.ErrInvalidConfig)
	case o.logger == nil:
		return fmt.Errorf("nil logger: %w", consumer.ErrInvalidConfig)
	case o.now == nil:
		return fmt.Errorf("nil clock: %w", consumer.ErrInvalidConfig)
	case slices.ContainsFunc(o.redrive, func(d time.Duration) bool { return d <= 0 }):
		return fmt.Errorf("redrive steps %v: %w", o.redrive, consumer.ErrInvalidConfig)
	}
	return nil
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
//
// The lease holder redrives Parked Streams on the WithRedrive schedule.
type Reactor struct {
	name     string
	actor    string
	handler  consumer.Handler
	opts     options
	retry    backoff.Retry
	parks    parkStore
	codec    *eventstore.Codec
	consumer *consumer.Consumer
	logger   *slog.Logger
	inst     instruments
	// parked wakes the auto-redrive loop after a park.
	parked chan struct{}
	// unparked holds the parks whose write failed, so a retry repeats only
	// the write and not the reaction.
	unparked pendingParks
}

// pendingParks are parks, by EventID, whose reaction failed for good but
// whose write to the park list did not succeed yet.
type pendingParks struct {
	mu sync.Mutex
	m  map[uuid.UUID]parkInput
}

func (p *pendingParks) get(id uuid.UUID) (parkInput, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	in, ok := p.m[id]
	return in, ok
}

func (p *pendingParks) put(in parkInput) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = make(map[uuid.UUID]parkInput)
	}
	p.m[in.env.EventID] = in
}

func (p *pendingParks) remove(id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, id)
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
	if err := o.validate(); err != nil {
		return nil, fmt.Errorf("new reactor %q: %w", cfg.Name, err)
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
		inst:    newInstruments(telemetry.Providers{MeterProvider: o.meterProvider}.Meter(scope), cfg.Name),
		parked:  make(chan struct{}, 1),
	}
	r.parks = newParkStore(backend, cfg.Name)
	r.codec = backend.Codec

	hook, ok := consumerhook.ReactorOption(consumerhook.ReactorOptions{
		SeedAtHead:   cfg.StartAt == FromNow,
		DecodeFailed: r.decodeFailed,
		Leased:       r.leased,
		Drained:      r.redriveDue,
	}).(consumer.Option)
	if !ok {
		return nil, fmt.Errorf("new reactor %q: runtime hook is not a consumer.Option: %w", cfg.Name, consumer.ErrInvalidConfig)
	}
	var handler consumer.Handler
	if cfg.Handler != nil {
		handler = consumer.HandlerFunc(r.handle)
	}
	c, err := consumer.New(rt, consumer.Config{
		Name:       cfg.Name,
		EventTypes: filter(cfg),
		Mode:       consumer.OwnTransaction,
		Handler:    handler,
	}, consumer.WithRetry(o.retry), consumer.WithLogger(o.logger), consumer.WithEventSpans(true),
		consumer.WithMeterProvider(o.meterProvider), hook)
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
// or reacts and Parks the Stream when the reaction fails for good. When
// only the park write failed before, it retries just that write.
func (r *Reactor) handle(ctx context.Context, env eventstore.Envelope) error {
	if in, ok := r.unparked.get(env.EventID); ok {
		return r.park(ctx, in)
	}
	held, err := r.parks.holdIfParked(ctx, parkInput{env: env, at: r.now()})
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
	in := parkInput{env: env, cause: err.Error(), attempts: attempts}
	r.unparked.put(in)
	return r.park(ctx, in)
}

// decodeFailed Parks an undecodable event at once.
func (r *Reactor) decodeFailed(ctx context.Context, f consumerhook.DecodeFailure) error {
	in := parkInput{env: f.Env, payload: &f.Payload, at: r.now()}
	held, err := r.parks.holdIfParked(ctx, in)
	if err != nil {
		return fmt.Errorf("check park list: %w", err)
	}
	if held {
		return nil
	}
	in.cause, in.attempts = f.Err.Error(), 1
	return r.park(ctx, in)
}

func (r *Reactor) park(ctx context.Context, in parkInput) error {
	in.at = r.now()
	if d, ok := r.opts.redrive.Delay(1, nil); ok {
		in.nextAttempt = in.at.Add(d)
	}
	if err := r.parks.park(ctx, in); err != nil {
		return fmt.Errorf("park event %s: %w", in.env.EventID, err)
	}
	r.unparked.remove(in.env.EventID)
	select {
	case r.parked <- struct{}{}:
	default:
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

func (r *Reactor) now() time.Time { return r.opts.now().UTC() }

func eventAttrs(env eventstore.Envelope) []any {
	return []any{
		telemetry.LogTenantID, env.TenantID,
		telemetry.LogStreamType, env.StreamType,
		telemetry.LogStreamID, env.StreamID,
		telemetry.LogEventID, env.EventID,
		telemetry.LogEventType, env.EventType,
	}
}
