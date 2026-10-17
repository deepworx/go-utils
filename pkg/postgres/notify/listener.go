// Package notify multiplexes Postgres LISTEN/NOTIFY over one connection per
// process.
//
// A Listener hijacks a single connection from the pool and LISTENs on every
// channel that has at least one Subscription. Delivery is best effort: when a
// Subscription's buffer is full, or the connection was lost and
// re-established, the Subscription receives a Notification with Gap set and
// must assume it missed notifications.
//
// PgBouncer in transaction mode is not supported, because LISTEN needs a real
// session.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric"

	"github.com/deepworx/go-utils/internal/backoff"
	"github.com/deepworx/go-utils/internal/telemetry"
)

const (
	scope         = "github.com/deepworx/go-utils/pkg/postgres/notify"
	defaultBuffer = 64
	closeTimeout  = 5 * time.Second
)

var reconnectDelay = backoff.Retry{MaxAttempts: math.MaxInt, Base: 100 * time.Millisecond, Max: 5 * time.Second}

// Notification is one delivery to a Subscription.
type Notification struct {
	// Payload is the NOTIFY payload. It is empty for a Gap.
	Payload string
	// Gap reports that notifications may have been lost, because the
	// Subscription's buffer was full or the connection was re-established.
	Gap bool
}

// Config configures a Listener.
type Config struct {
	// Buffer is the capacity of each Subscription's channel. Default 64.
	Buffer int
	// Logger receives connection errors. Default slog.Default().
	Logger *slog.Logger
}

// Option configures optional Listener dependencies.
type Option func(*telemetry.Providers)

// WithMeterProvider records the notify.reconnects and notify.gaps counters
// with mp instead of the global meter provider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(p *telemetry.Providers) { p.MeterProvider = mp }
}

// Listener owns the process's single LISTEN connection and fans incoming
// notifications out to Subscriptions.
type Listener struct {
	pool       *pgxpool.Pool
	buffer     int
	logger     *slog.Logger
	reconnects metric.Int64Counter
	gaps       metric.Int64Counter

	running atomic.Bool
	wake    chan struct{}
	onConn  chan func(context.Context, *pgx.Conn) // runs probes on the connection in tests

	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}
}

// NewListener returns a Listener that hijacks its connection from pool. It
// does nothing until Run is called.
func NewListener(pool *pgxpool.Pool, cfg Config, opts ...Option) (*Listener, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if cfg.Buffer < 0 {
		return nil, fmt.Errorf("notify: buffer %d is negative", cfg.Buffer)
	}
	if cfg.Buffer == 0 {
		cfg.Buffer = defaultBuffer
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	var providers telemetry.Providers
	for _, opt := range opts {
		opt(&providers)
	}
	meter := providers.Meter(scope)
	reconnects, err := telemetry.NotifyReconnects.Int64Counter(meter)
	if err != nil {
		return nil, fmt.Errorf("notify: create reconnects counter: %w", err)
	}
	gaps, err := telemetry.NotifyGaps.Int64Counter(meter)
	if err != nil {
		return nil, fmt.Errorf("notify: create gaps counter: %w", err)
	}
	return &Listener{
		pool:       pool,
		buffer:     cfg.Buffer,
		logger:     cfg.Logger,
		reconnects: reconnects,
		gaps:       gaps,
		wake:       make(chan struct{}, 1),
		onConn:     make(chan func(context.Context, *pgx.Conn)),
		subs:       make(map[string]map[*Subscription]struct{}),
	}, nil
}

// Subscribe returns a Subscription for channel. The first Subscription of a
// channel makes the running Listener LISTEN on it; notifications sent before
// that LISTEN took effect are not delivered. Callers must Close the
// Subscription when done.
func (l *Listener) Subscribe(channel string) (*Subscription, error) {
	if err := CheckChannel(channel); err != nil {
		return nil, err
	}
	sub := &Subscription{
		listener: l,
		channel:  channel,
		ch:       make(chan Notification, l.buffer),
	}
	l.mu.Lock()
	if l.subs[channel] == nil {
		l.subs[channel] = make(map[*Subscription]struct{})
	}
	l.subs[channel][sub] = struct{}{}
	l.mu.Unlock()
	l.signal()
	return sub, nil
}

// Run holds the LISTEN connection until ctx ends and then returns nil. A lost
// connection is replaced by a newly hijacked one after a backoff, and every
// Subscription then receives a Gap. Run returns ErrRunning while another Run
// of the same Listener is active.
func (l *Listener) Run(ctx context.Context) error {
	if !l.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer l.running.Store(false)

	reconnect := false
	failed := 0
	for {
		connected, err := l.session(ctx, reconnect)
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			reconnect = true
			failed = 0
		}
		failed++
		delay, _ := reconnectDelay.Delay(failed, nil)
		l.logger.WarnContext(ctx, "notify: listen connection failed", slog.Int(telemetry.LogAttempt, failed), slog.String(telemetry.LogError, err.Error()))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// session hijacks a connection and serves it until ctx ends or the
// connection fails. connected reports whether the connection was established.
func (l *Listener) session(ctx context.Context, reconnect bool) (connected bool, err error) {
	conn, err := l.hijack(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	listening := make(map[string]struct{})
	if err := l.sync(ctx, conn, listening); err != nil {
		return true, err
	}
	if reconnect {
		l.reconnects.Add(ctx, 1)
		l.broadcastGap(ctx)
	}
	for {
		n, w, err := l.wait(ctx, conn)
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		if err != nil {
			return true, fmt.Errorf("wait for notification: %w", err)
		}
		if n != nil {
			l.dispatch(ctx, n.Channel, n.Payload)
		}
		if w != nil && w.connFn != nil {
			w.connFn(ctx, conn)
		}
		if err := l.sync(ctx, conn, listening); err != nil {
			return true, err
		}
	}
}

func (l *Listener) hijack(ctx context.Context) (*pgx.Conn, error) {
	pc, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire listen connection: %w", err)
	}
	return pc.Hijack(), nil
}

// wakeup is what ended a wait other than a notification or an error.
type wakeup struct {
	// connFn is a queued request to run on the connection, or nil when the
	// subscriber set changed.
	connFn func(context.Context, *pgx.Conn)
}

// wait blocks until a notification arrives, ctx ends, the subscriber set
// changes or a connection request is queued. It returns an error only when
// neither a notification nor a wakeup was received.
func (l *Listener) wait(ctx context.Context, conn *pgx.Conn) (*pgconn.Notification, *wakeup, error) {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	woke := make(chan *wakeup, 1)
	go func() {
		defer close(woke)
		select {
		case <-l.wake:
			woke <- &wakeup{}
		case fn := <-l.onConn:
			woke <- &wakeup{connFn: fn}
		case <-done:
			return
		}
		cancel()
	}()

	n, err := conn.WaitForNotification(waitCtx)
	close(done)
	w := <-woke
	if n != nil || (w != nil && ctx.Err() == nil) {
		return n, w, nil
	}
	return nil, nil, err
}

// sync issues LISTEN for channels that gained their first Subscription and
// UNLISTEN for channels that lost their last one.
func (l *Listener) sync(ctx context.Context, conn *pgx.Conn, listening map[string]struct{}) error {
	l.mu.Lock()
	var add, remove []string
	for channel := range l.subs {
		if _, ok := listening[channel]; !ok {
			add = append(add, channel)
		}
	}
	for channel := range listening {
		if _, ok := l.subs[channel]; !ok {
			remove = append(remove, channel)
		}
	}
	l.mu.Unlock()

	for _, channel := range add {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
			return fmt.Errorf("listen %q: %w", channel, err)
		}
		listening[channel] = struct{}{}
	}
	for _, channel := range remove {
		if _, err := conn.Exec(ctx, "UNLISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
			return fmt.Errorf("unlisten %q: %w", channel, err)
		}
		delete(listening, channel)
	}
	return nil
}

func (l *Listener) dispatch(ctx context.Context, channel, payload string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for sub := range l.subs[channel] {
		l.deliver(ctx, sub, Notification{Payload: payload})
	}
}

func (l *Listener) broadcastGap(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, subs := range l.subs {
		for sub := range subs {
			l.deliver(ctx, sub, Notification{Gap: true})
		}
	}
}

// deliver sends n without blocking. A full buffer is drained and replaced by
// a single Gap, which supersedes everything it dropped. The caller holds
// l.mu, so sub.ch is neither closed nor written concurrently.
func (l *Listener) deliver(ctx context.Context, sub *Subscription, n Notification) {
	select {
	case sub.ch <- n:
		if n.Gap {
			l.gaps.Add(ctx, 1)
		}
		return
	default:
	}
	for drained := false; !drained; {
		select {
		case <-sub.ch:
		default:
			drained = true
		}
	}
	sub.ch <- Notification{Gap: true}
	l.gaps.Add(ctx, 1)
}

func (l *Listener) unsubscribe(sub *Subscription) {
	l.mu.Lock()
	subs, ok := l.subs[sub.channel]
	if _, found := subs[sub]; !ok || !found {
		l.mu.Unlock()
		return
	}
	delete(subs, sub)
	if len(subs) == 0 {
		delete(l.subs, sub.channel)
	}
	close(sub.ch)
	l.mu.Unlock()
	l.signal()
}

func (l *Listener) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Subscription receives the notifications of one channel.
type Subscription struct {
	listener *Listener
	channel  string
	ch       chan Notification
}

// C returns the channel notifications are delivered on. It is closed by
// Close.
func (s *Subscription) C() <-chan Notification { return s.ch }

// Close ends the Subscription and closes C. The last Close of a channel
// makes the Listener UNLISTEN it. Close is idempotent.
func (s *Subscription) Close() { s.listener.unsubscribe(s) }
