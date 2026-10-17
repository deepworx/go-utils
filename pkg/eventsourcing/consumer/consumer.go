package consumer

import (
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/internal/backoff"
	"github.com/deepworx/go-utils/internal/telemetry"
)

const scope = "github.com/deepworx/go-utils/pkg/eventsourcing/consumer"

// Mode selects the delivery guarantee of a Consumer.
type Mode int

const (
	// Transactional runs the handlers of a batch and the Cursor update in one
	// Cursor transaction, so every event takes effect exactly once. The
	// handler writes through the transaction in ctx (postgres.Conn, or
	// postgres.Stage in memory).
	Transactional Mode = iota + 1
	// OwnTransaction runs each handler on its own and advances the Cursor
	// after each event, so delivery is at least once.
	OwnTransaction
)

// Config configures a Consumer.
type Config struct {
	// Name identifies the Consumer and its Cursor; unique per Runtime.
	Name string
	// EventTypes is the filter. When empty, a Handler with an EventTypes
	// method (a Router) provides it; otherwise every event is delivered.
	EventTypes []string
	Mode       Mode
	Handler    Handler
}

// Retry is the retry schedule of a failing event: up to MaxAttempts
// attempts in total, with jittered exponential delays from Base up to Max.
type Retry struct {
	MaxAttempts int
	Base        time.Duration
	Max         time.Duration
}

// Option configures a Consumer.
type Option func(*options)

type options struct {
	batchSize      int
	pollInterval   time.Duration
	retry          Retry
	logger         *slog.Logger
	eventSpans     bool
	tracerProvider trace.TracerProvider
}

// WithBatchSize sets how many events one batch reads. The default is 100.
func WithBatchSize(n int) Option {
	return func(o *options) { o.batchSize = n }
}

// WithPollInterval sets how long a running Consumer waits for new events
// before it polls again. The wait is jittered down by up to a fifth. The
// default is 1s.
func WithPollInterval(d time.Duration) Option {
	return func(o *options) { o.pollInterval = d }
}

// WithRetry sets the retry schedule. The default is 5 attempts with delays
// from 1s up to 30s.
func WithRetry(r Retry) Option {
	return func(o *options) { o.retry = r }
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// WithEventSpans turns the per-event "handle {event type}" spans on or off.
// They are off by default.
func WithEventSpans(on bool) Option {
	return func(o *options) { o.eventSpans = on }
}

// WithTracerProvider sets the tracer provider. The default is the OTel
// global.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProvider = tp }
}

func newOptions(opts []Option) options {
	o := options{
		batchSize:    100,
		pollInterval: time.Second,
		retry:        Retry{MaxAttempts: 5, Base: time.Second, Max: 30 * time.Second},
		logger:       slog.Default(),
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func (o options) validate() error {
	switch {
	case o.batchSize < 1:
		return fmt.Errorf("batch size %d: %w", o.batchSize, ErrInvalidConfig)
	case o.pollInterval <= 0:
		return fmt.Errorf("poll interval %s: %w", o.pollInterval, ErrInvalidConfig)
	case o.retry.MaxAttempts < 1 || o.retry.Base <= 0 || o.retry.Max < o.retry.Base:
		return fmt.Errorf("retry %+v: %w", o.retry, ErrInvalidConfig)
	case o.logger == nil:
		return fmt.Errorf("nil logger: %w", ErrInvalidConfig)
	}
	return nil
}

// Consumer is a named, Cursor-tracked reader of the event log. It reads the
// events matching its filter in Cursor order and hands each to its Handler.
// A failing event is retried and then stops the Consumer; it is never
// skipped.
type Consumer struct {
	name    string
	mode    Mode
	handler Handler
	opts    options
	retry   backoff.Retry
	port    port
	logger  *slog.Logger
	tracer  trace.Tracer
	running atomic.Bool
}

type eventTyper interface {
	EventTypes() []string
}

// New creates a Consumer on rt. It fails with ErrInvalidConfig for an empty
// Name, a nil Handler, an unknown Mode or invalid options, and with
// ErrDuplicate when rt already has a Consumer with that Name.
func New(rt Runtime, cfg Config, opts ...Option) (*Consumer, error) {
	o := newOptions(opts)
	if err := validate(cfg, o); err != nil {
		return nil, fmt.Errorf("new consumer %q: %w", cfg.Name, err)
	}
	types := cfg.EventTypes
	if typer, ok := cfg.Handler.(eventTyper); ok && len(types) == 0 {
		types = typer.EventTypes()
	}
	types = slices.Compact(slices.Sorted(slices.Values(types)))

	p, err := rt.attach(attachInput{name: cfg.Name, eventTypes: types, mode: cfg.Mode})
	if err != nil {
		return nil, fmt.Errorf("new consumer: %w", err)
	}
	return &Consumer{
		name:    cfg.Name,
		mode:    cfg.Mode,
		handler: cfg.Handler,
		opts:    o,
		retry:   backoff.Retry(o.retry),
		port:    p,
		logger:  o.logger.With(telemetry.LogConsumer, cfg.Name),
		tracer:  telemetry.Providers{TracerProvider: o.tracerProvider}.Tracer(scope),
	}, nil
}

func validate(cfg Config, o options) error {
	switch {
	case cfg.Name == "":
		return fmt.Errorf("empty name: %w", ErrInvalidConfig)
	case cfg.Handler == nil:
		return fmt.Errorf("nil handler: %w", ErrInvalidConfig)
	case cfg.Mode != Transactional && cfg.Mode != OwnTransaction:
		return fmt.Errorf("mode %d: %w", cfg.Mode, ErrInvalidConfig)
	case slices.Contains(cfg.EventTypes, ""):
		return fmt.Errorf("empty event type: %w", ErrInvalidConfig)
	}
	return o.validate()
}

// Name returns the Consumer's name.
func (c *Consumer) Name() string { return c.name }
