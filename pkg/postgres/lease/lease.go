// Package lease grants mutual exclusion between processes through Postgres
// session advisory locks, all held on one connection per process.
//
// Claims hijacks a single connection from the pool and takes every lock on
// it. Run keeps that connection alive and pings it every PingInterval. When
// the session ends, every held Lease is lost at once: its context is
// cancelled with cause ErrLost, and Run reconnects.
//
// Leases are not fences. A lost lease is only noticed at the next ping, and
// in that window another process can acquire the same lease while the old
// holder is still working. Every holder must therefore fence its own writes
// (for example with a compare-and-set or a generation column); holding the
// lease only prevents duplicate work, it never guarantees correctness.
//
// Locks live in the two-int advisory space (fnv32a(Namespace),
// fnv32a(Name)). PgBouncer in transaction mode is not supported.
package lease

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric"

	"github.com/deepworx/go-utils/internal/backoff"
	"github.com/deepworx/go-utils/internal/telemetry"
)

const scope = "github.com/deepworx/go-utils/pkg/postgres/lease"

const (
	defaultPingInterval = 5 * time.Second
	// statementTimeout bounds acquire, unlock and close statements so that a
	// cancelled caller never interrupts a statement on the shared connection.
	statementTimeout = 5 * time.Second
)

var reconnectBackoff = backoff.Retry{MaxAttempts: math.MaxInt, Base: 100 * time.Millisecond, Max: 5 * time.Second}

var (
	// ErrLost is the cause of a lease context cancelled because the lock
	// connection ended or Run stopped.
	ErrLost = errors.New("lease: lost")
	// ErrHeld is returned by TryAcquire when another session, or this
	// process, already holds the lock.
	ErrHeld = errors.New("lease: held")
	// ErrNotConnected is returned by TryAcquire while Run has no connection.
	ErrNotConnected = errors.New("lease: not connected")
	// ErrEmptyName is returned by TryAcquire for a Key without a Name.
	ErrEmptyName = errors.New("lease: empty name")
	// ErrRunning is returned by Run when it is already running.
	ErrRunning = errors.New("lease: already running")
)

// Key identifies a lock. Namespace groups related locks, such as all locks
// of one package in one schema.
type Key struct {
	Namespace string
	Name      string
}

func (k Key) String() string { return k.Namespace + "/" + k.Name }

// Config configures Claims.
type Config struct {
	// PingInterval is how often the lock connection is checked. Lease loss
	// is noticed at most this late. Defaults to 5s.
	PingInterval time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Option configures Claims beyond Config.
type Option func(*telemetry.Providers)

// WithMeterProvider records metrics with mp instead of the global provider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(p *telemetry.Providers) { p.MeterProvider = mp }
}

// Claims takes session advisory locks on one connection hijacked from the
// pool. Run must be running for TryAcquire to succeed.
type Claims struct {
	pool         *pgxpool.Pool
	pingInterval time.Duration
	logger       *slog.Logger
	metrics      metrics
	running      atomic.Bool

	mu   sync.Mutex
	conn *pgx.Conn
	held map[Key]*Lease
}

// NewClaims returns Claims that take their connection from pool.
func NewClaims(pool *pgxpool.Pool, cfg Config, opts ...Option) *Claims {
	var providers telemetry.Providers
	for _, opt := range opts {
		opt(&providers)
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = defaultPingInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	m, err := newMetrics(providers.Meter(scope))
	if err != nil {
		cfg.Logger.Warn("lease: create metrics", slog.Any(telemetry.LogError, err))
	}
	return &Claims{
		pool:         pool,
		pingInterval: cfg.PingInterval,
		logger:       cfg.Logger,
		metrics:      m,
		held:         make(map[Key]*Lease),
	}
}

// Run hijacks the lock connection, pings it every PingInterval and
// reconnects after it is lost. When ctx ends it closes the connection,
// cancels every held lease with ErrLost and returns nil.
func (c *Claims) Run(ctx context.Context) error {
	if !c.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer c.running.Store(false)

	connected := false
	failed := 0
	for {
		conn, err := c.connect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			failed++
			c.logger.WarnContext(ctx, "lease: connect", slog.Int(telemetry.LogAttempt, failed), slog.Any(telemetry.LogError, err))
			delay, _ := reconnectBackoff.Delay(failed, nil)
			if !sleep(ctx, delay) {
				return nil
			}
			continue
		}
		if connected {
			c.metrics.reconnects.Add(ctx, 1)
		}
		connected = true
		failed = 0

		c.attach(conn)
		err = c.watch(ctx, conn)
		shutdown := ctx.Err() != nil
		c.detach(!shutdown)
		if shutdown {
			return nil
		}
		c.logger.WarnContext(ctx, "lease: connection lost", slog.Any(telemetry.LogError, err))
	}
}

func (c *Claims) connect(ctx context.Context) (*pgx.Conn, error) {
	pc, err := c.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire lock connection: %w", err)
	}
	return pc.Hijack(), nil
}

func (c *Claims) attach(conn *pgx.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn = conn
}

// watch pings conn until a ping fails or ctx ends.
func (c *Claims) watch(ctx context.Context, conn *pgx.Conn) error {
	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.ping(ctx, conn); err != nil {
				return err
			}
		}
	}
}

func (c *Claims) ping(ctx context.Context, conn *pgx.Conn) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	pingCtx, cancel := context.WithTimeout(ctx, c.pingInterval)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		return fmt.Errorf("ping lock connection: %w", err)
	}
	return nil
}

// detach closes the connection and cancels every held lease with ErrLost.
// countLost is false on shutdown, where losing the leases is intended.
func (c *Claims) detach(countLost bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx := context.Background()
	for key, l := range c.held {
		l.cancel(ErrLost)
		delete(c.held, key)
		c.metrics.held.Add(ctx, -1)
		if countLost {
			c.metrics.lost.Add(ctx, 1)
		}
	}

	closeCtx, cancel := context.WithTimeout(ctx, statementTimeout)
	defer cancel()
	if err := c.conn.Close(closeCtx); err != nil {
		c.logger.Debug("lease: close lock connection", slog.Any(telemetry.LogError, err))
	}
	c.conn = nil
}

// TryAcquire takes the lock for key without waiting. The returned context
// is derived from ctx and is cancelled with cause ErrLost when the lease is
// lost; Release cancels it too. It fails with ErrHeld when another session
// or this process holds the lock, with ErrNotConnected while Run has no
// connection, and with ErrEmptyName when key has no Name.
func (c *Claims) TryAcquire(ctx context.Context, key Key) (context.Context, *Lease, error) {
	if key.Name == "" {
		return nil, nil, ErrEmptyName
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil, nil, ErrNotConnected
	}
	if _, ok := c.held[key]; ok {
		return nil, nil, fmt.Errorf("%w by this process: %s", ErrHeld, key)
	}

	stmtCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statementTimeout)
	defer cancel()
	var locked bool
	err := c.conn.QueryRow(stmtCtx, "SELECT pg_try_advisory_lock($1, $2)", lockID(key.Namespace), lockID(key.Name)).Scan(&locked)
	if err != nil {
		return nil, nil, fmt.Errorf("lease: acquire %s: %w", key, err)
	}
	if !locked {
		return nil, nil, fmt.Errorf("%w by another session: %s", ErrHeld, key)
	}

	leaseCtx, cancelLease := context.WithCancelCause(ctx)
	l := &Lease{claims: c, key: key, cancel: cancelLease}
	c.held[key] = l
	c.metrics.held.Add(ctx, 1)
	return leaseCtx, l, nil
}

// Lease is a held lock. Its holder must stop working when the context
// returned by TryAcquire ends.
type Lease struct {
	claims *Claims
	key    Key
	cancel context.CancelCauseFunc
}

// Release cancels the lease context and unlocks the lock. It is idempotent
// and a no-op after the lease was lost. The unlock runs under its own 5s
// timeout, independent of any caller context.
func (l *Lease) Release() {
	c := l.claims
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.held[l.key] != l {
		return
	}
	delete(c.held, l.key)
	l.cancel(nil)

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	c.metrics.held.Add(ctx, -1)

	var unlocked bool
	err := c.conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1, $2)", lockID(l.key.Namespace), lockID(l.key.Name)).Scan(&unlocked)
	switch {
	case err != nil:
		c.logger.WarnContext(ctx, "lease: unlock", slog.String("lease", l.key.String()), slog.Any(telemetry.LogError, err))
	case !unlocked:
		c.logger.WarnContext(ctx, "lease: unlock found no lock", slog.String("lease", l.key.String()))
	}
}

// lockID maps s to one int4 half of a two-int advisory lock key.
func lockID(s string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return int32(h.Sum32()) //nolint:gosec // wrapping into the signed int4 range is intended
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
