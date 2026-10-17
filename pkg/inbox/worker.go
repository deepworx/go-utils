package inbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/internal/backoff"
	"github.com/deepworx/go-utils/internal/telemetry"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

const (
	scope = "github.com/deepworx/go-utils/pkg/inbox"

	defaultConcurrency  = 1
	defaultTimeout      = 60 * time.Second
	defaultPollInterval = time.Second
	defaultGaugeEvery   = 15 * time.Second
	// leaseMargin extends a claim beyond the handler Timeout, so a slow but
	// live handler settles before another Worker may claim its message.
	leaseMargin   = 30 * time.Second
	settleTimeout = 5 * time.Second
)

// DefaultRetries is the retry schedule used when Config.Retries is nil.
// After the last step a failing message is Parked.
var DefaultRetries = []time.Duration{
	2 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute,
	time.Hour, 6 * time.Hour, 24 * time.Hour,
}

// Delivery is one attempt to handle a Message.
type Delivery struct {
	Message
	// ID is the message's row ID.
	ID int64
	// Attempt counts the attempts including this one, starting at 1.
	Attempt int
	// ReceivedAt is when the message was inserted.
	ReceivedAt time.Time
}

// Handler handles one Delivery. Its result settles the message:
//
//   - nil: done
//   - Ignore(reason): ignored
//   - Permanent(err): Parked
//   - any other error, or a panic: retried after the next step of the retry
//     schedule, and Parked once the schedule is spent
//
// The ctx carries the Actor system:inbox:<source>, the Correlation
// inbox:<source>:<key>, the message's Tenant when it has one, and the
// Worker's Timeout as its deadline.
type Handler func(ctx context.Context, d Delivery) error

// Config configures a Worker.
type Config struct {
	// Source is the Source whose messages the Worker handles. Required.
	Source string
	// Handle handles each message. Required.
	Handle Handler
	// Concurrency is the number of messages handled at once. Default 1.
	Concurrency int
	// Timeout bounds one Handle call. A claim lasts Timeout+30s. Default 60s.
	Timeout time.Duration
	// Retries are the delays after each failed attempt; once spent, the
	// message is Parked. Nil uses DefaultRetries; an empty non-nil slice
	// Parks on the first failure. Delays are jittered down by up to a fifth.
	Retries []time.Duration
}

type options struct {
	providers    telemetry.Providers
	logger       *slog.Logger
	listener     *notify.Listener
	claims       *lease.Claims
	pollInterval time.Duration
}

// Option configures a Worker beyond Config.
type Option func(*options)

// WithLogger sets the logger. Default slog.Default(). The Source is bound
// to every record.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithTracerProvider records spans with tp instead of the global provider.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.providers.TracerProvider = tp }
}

// WithMeterProvider records metrics with mp instead of the global provider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.providers.MeterProvider = mp }
}

// WithListener makes Run wake up on the <schema>.inbox notify channel as
// soon as a message of the Worker's Source is inserted. Without it, Run
// only polls.
func WithListener(l *notify.Listener) Option { return func(o *options) { o.listener = l } }

// WithClaims makes Run report the inbox.messages and
// inbox.pending.oldest_age gauges while it holds the Source's telemetry
// lease, so that only one pod reports them. Without it, the gauges are not
// reported.
func WithClaims(c *lease.Claims) Option { return func(o *options) { o.claims = c } }

// WithPollInterval sets how often Run looks for due messages without a
// notification, jittered down by up to a fifth. Default 1s.
func WithPollInterval(d time.Duration) Option { return func(o *options) { o.pollInterval = d } }

// Worker claims and handles the messages of one Source.
type Worker struct {
	store       *Store
	source      string
	handle      Handler
	concurrency int
	timeout     time.Duration
	retries     backoff.Steps

	logger       *slog.Logger
	tracer       trace.Tracer
	metrics      metrics
	listener     *notify.Listener
	claims       *lease.Claims
	pollInterval time.Duration
	gaugeEvery   time.Duration

	running atomic.Bool
}

// NewWorker returns a Worker for cfg.Source on store. It fails with
// ErrInvalidConfig when Source or Handle is missing.
func NewWorker(store *Store, cfg Config, opts ...Option) (*Worker, error) {
	if cfg.Source == "" {
		return nil, fmt.Errorf("%w: empty source", ErrInvalidConfig)
	}
	if cfg.Handle == nil {
		return nil, fmt.Errorf("%w: nil handle", ErrInvalidConfig)
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Retries == nil {
		cfg.Retries = DefaultRetries
	}
	o := options{logger: slog.Default(), pollInterval: defaultPollInterval}
	for _, opt := range opts {
		opt(&o)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	if o.pollInterval <= 0 {
		o.pollInterval = defaultPollInterval
	}
	logger := o.logger.With(slog.String(telemetry.LogSource, cfg.Source))
	m, err := newMetrics(o.providers.Meter(scope))
	if err != nil {
		logger.Warn("inbox: create metrics", slog.Any(telemetry.LogError, err))
	}
	return &Worker{
		store:        store,
		source:       cfg.Source,
		handle:       cfg.Handle,
		concurrency:  cfg.Concurrency,
		timeout:      cfg.Timeout,
		retries:      backoff.Steps(append([]time.Duration(nil), cfg.Retries...)),
		logger:       logger,
		tracer:       o.providers.Tracer(scope),
		metrics:      m,
		listener:     o.listener,
		claims:       o.claims,
		pollInterval: o.pollInterval,
		gaugeEvery:   defaultGaugeEvery,
	}, nil
}

// Drain handles every message of the Source that is due when it is called,
// and messages that become due while it runs, until none is due. It
// returns ErrRunning while Run or another Drain of this Worker is active.
func (w *Worker) Drain(ctx context.Context) error {
	if !w.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer w.running.Store(false)
	return w.process(ctx)
}

// Run handles due messages until ctx ends and then returns nil, after the
// messages in flight have been settled. It looks for due messages on every
// notification for its Source (with WithListener) and at least every poll
// interval. It returns ErrRunning while Drain or another Run of this Worker
// is active.
func (w *Worker) Run(ctx context.Context) error {
	if !w.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer w.running.Store(false)

	var wake <-chan notify.Notification
	if w.listener != nil {
		sub, err := w.listener.Subscribe(w.store.channel)
		if err != nil {
			return fmt.Errorf("inbox: subscribe %s: %w", w.store.channel, err)
		}
		defer sub.Close()
		wake = sub.C()
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	if w.claims != nil {
		wg.Go(func() { w.reportGauges(ctx) })
	}

	for {
		if err := w.process(ctx); err != nil && ctx.Err() == nil {
			w.logger.WarnContext(ctx, "inbox: process", slog.Any(telemetry.LogError, err))
		}
		if !w.wait(ctx, wake) {
			return nil
		}
	}
}

// wait blocks until a notification for the Source or a Gap arrives, or the
// poll interval elapses. It reports false when ctx ended.
func (w *Worker) wait(ctx context.Context, wake <-chan notify.Notification) bool {
	timer := time.NewTimer(backoff.Poll(w.pollInterval, nil))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case n := <-wake:
			if n.Gap || n.Payload == w.source {
				return true
			}
		}
	}
}

// process claims and handles due messages with up to Concurrency handlers
// in flight, until a claim finds nothing due. It returns after every
// handler has settled.
func (w *Worker) process(ctx context.Context) error {
	slots := make(chan struct{}, w.concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		free := 1
	fill:
		for free < w.concurrency {
			select {
			case slots <- struct{}{}:
				free++
			default:
				break fill
			}
		}

		batch, err := w.store.claim(ctx, claimInput{Source: w.source, Limit: free, Lease: w.timeout + leaseMargin})
		for range free - len(batch) {
			<-slots
		}
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, c := range batch {
			wg.Go(func() {
				defer func() { <-slots }()
				w.handleClaimed(ctx, c)
			})
		}
	}
}

func (w *Worker) handleClaimed(ctx context.Context, c claimed) {
	attrs := []attribute.KeyValue{
		telemetry.AttrSource.String(w.source),
		telemetry.AttrAttempt.Int(c.Attempt),
	}
	if c.Tenant != "" {
		attrs = append(attrs, telemetry.AttrTenantID.String(c.Tenant))
	}
	spanOpts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attrs...),
	}
	if link, ok := storedLink(c.traceContext); ok {
		spanOpts = append(spanOpts, trace.WithLinks(link))
	}
	spanCtx, span := w.tracer.Start(ctx, telemetry.SpanInboxProcess(w.source), spanOpts...)
	defer span.End()

	start := time.Now()
	err := w.call(spanCtx, c.Delivery)
	w.metrics.handleDuration.Record(ctx, time.Since(start).Seconds(),
		metric.WithAttributes(telemetry.AttrSource.String(w.source)))

	st := w.settlement(c, err)
	span.SetAttributes(telemetry.AttrOutcome.String(outcome(st)))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(telemetry.AttrErrorType.String(fmt.Sprintf("%T", err)))
	}

	logger := w.logger.With(slog.Int64("message_id", c.ID), slog.Int(telemetry.LogAttempt, c.Attempt))
	if c.Tenant != "" {
		logger = logger.With(slog.String(telemetry.LogTenantID, c.Tenant))
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	settled, serr := w.store.settle(settleCtx, st)
	switch {
	case serr != nil:
		logger.ErrorContext(spanCtx, "inbox: settle", slog.Any(telemetry.LogError, serr))
		return
	case !settled:
		logger.WarnContext(spanCtx, "inbox: claim lost before settle, result dropped", slog.String("outcome", outcome(st)))
		return
	}
	w.metrics.handled.Add(ctx, 1, metric.WithAttributes(
		telemetry.AttrSource.String(w.source),
		telemetry.AttrOutcome.String(outcome(st)),
	))
	w.logOutcome(spanCtx, logger, st)
}

func (w *Worker) logOutcome(ctx context.Context, logger *slog.Logger, st settlement) {
	switch outcome(st) {
	case outcomeRetry:
		logger.WarnContext(ctx, "inbox: handler failed, retrying", slog.Time("retry_at", st.RetryAt), slog.String(telemetry.LogError, st.Error))
	case string(StatusParked):
		logger.ErrorContext(ctx, "inbox: message parked", slog.String(telemetry.LogError, st.Error))
	case string(StatusIgnored):
		logger.DebugContext(ctx, "inbox: message ignored", slog.String("reason", st.Error))
	}
}

const outcomeRetry = "retry"

func outcome(st settlement) string {
	if st.Status == StatusPending {
		return outcomeRetry
	}
	return string(st.Status)
}

// call runs the Handler with the message's context and Timeout and turns a
// panic into an error.
func (w *Worker) call(ctx context.Context, d Delivery) (err error) {
	ctx = ctxutil.WithActor(ctx, "system:inbox:"+d.Source)
	ctx = ctxutil.WithCorrelationID(ctx, "inbox:"+d.Source+":"+d.Key)
	if d.Tenant != "" {
		ctx = ctxutil.WithTenantID(ctx, d.Tenant)
	}
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = &panicError{value: r}
			w.logger.ErrorContext(ctx, "inbox: handler panic",
				slog.Int64("message_id", d.ID),
				slog.String(telemetry.LogError, err.Error()),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	return w.handle(ctx, d)
}

type panicError struct{ value any }

func (e *panicError) Error() string { return fmt.Sprintf("inbox: handler panic: %v", e.value) }

// settlement maps a Handler result to the message's next state.
func (w *Worker) settlement(c claimed, err error) settlement {
	st := settlement{ID: c.ID, Generation: c.generation}
	var (
		ignore    *ignoreError
		permanent *permanentError
	)
	switch {
	case err == nil:
		st.Status = StatusDone
	case errors.As(err, &ignore):
		st.Status, st.Error = StatusIgnored, ignore.reason
	case errors.As(err, &permanent):
		st.Status, st.Error = StatusParked, permanent.err.Error()
	default:
		st.Error = err.Error()
		delay, ok := w.retries.Delay(c.Attempt, nil)
		if !ok {
			st.Status = StatusParked
			break
		}
		st.Status, st.RetryAt = StatusPending, w.store.now().Add(delay)
	}
	return st
}

func storedLink(traceContext string) (trace.Link, bool) {
	if traceContext == "" {
		return trace.Link{}, false
	}
	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": traceContext})
	sc := trace.SpanContextFromContext(ctx)
	return trace.Link{SpanContext: sc}, sc.IsValid()
}

// reportGauges reports the Source's gauges every gaugeEvery while this
// process holds the Source's telemetry lease, retrying the lease on each
// tick until ctx ends.
func (w *Worker) reportGauges(ctx context.Context) {
	ticker := time.NewTicker(w.gaugeEvery)
	defer ticker.Stop()
	key := lease.Key{Namespace: w.store.channel, Name: "telemetry:" + w.source}
	for {
		w.reportWhileLeased(ctx, key, ticker.C)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) reportWhileLeased(ctx context.Context, key lease.Key, tick <-chan time.Time) {
	leaseCtx, held, err := w.claims.TryAcquire(ctx, key)
	if err != nil {
		if !errors.Is(err, lease.ErrHeld) && !errors.Is(err, lease.ErrNotConnected) {
			w.logger.WarnContext(ctx, "inbox: acquire telemetry lease", slog.Any(telemetry.LogError, err))
		}
		return
	}
	defer held.Release()
	for {
		if err := w.recordGauges(leaseCtx); err != nil && leaseCtx.Err() == nil {
			w.logger.WarnContext(ctx, "inbox: record gauges", slog.Any(telemetry.LogError, err))
		}
		select {
		case <-leaseCtx.Done():
			return
		case <-tick:
		}
	}
}

func (w *Worker) recordGauges(ctx context.Context) error {
	counts, err := w.store.counts(ctx, w.source)
	if err != nil {
		return err
	}
	source := telemetry.AttrSource.String(w.source)
	for status, n := range counts.ByStatus {
		w.metrics.messages.Record(ctx, n, metric.WithAttributes(source, telemetry.AttrStatus.String(string(status))))
	}
	w.metrics.oldestAge.Record(ctx, counts.OldestPending.Seconds(), metric.WithAttributes(source))
	return nil
}
