// Package postgres provides database access utilities with tracing integration.
//
// It offers pool initialization with tracing, health checks, connection metrics,
// and transaction management with auto-rollback.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/deepworx/go-utils/pkg/shutdown"
	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds configuration for PostgreSQL pool initialization.
type Config struct {
	// DSN is the PostgreSQL connection string.
	// Required. Example: "postgres://user:pass@localhost:5432/db?sslmode=disable"
	DSN string

	// MaxConns is the maximum number of connections in the pool.
	// Defaults to 10 if zero.
	MaxConns int32

	// MinConns is the minimum number of connections to keep open.
	// Defaults to 2 if zero.
	MinConns int32

	// MaxConnLifetime is the maximum lifetime of a connection.
	// Defaults to 1 hour if zero.
	MaxConnLifetime time.Duration

	// MaxConnIdleTime is the maximum time a connection can be idle.
	// Defaults to 30 minutes if zero.
	MaxConnIdleTime time.Duration

	// HealthCheckPeriod is the interval between health checks.
	// Defaults to 1 minute if zero.
	HealthCheckPeriod time.Duration
}

// NewPool creates a new PostgreSQL connection pool with tracing.
// It registers a shutdown handler to close the pool gracefully.
// Returns *pgxpool.Pool directly for sqlc compatibility.
func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("create postgres pool: %w", ErrDSNRequired)
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}

	applyDefaults(poolCfg, cfg)

	poolCfg.ConnConfig.Tracer = otelpgx.NewTracer()

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	if err := registerMetrics(pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("register postgres metrics: %w", err)
	}

	shutdown.Register(func(_ context.Context) error {
		pool.Close()
		return nil
	})

	return pool, nil
}

// Ping verifies database connectivity.
// Returns nil if the database is reachable, error otherwise.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}
	return nil
}

// WithTx executes fn within a database transaction.
// The transaction is committed if fn returns nil; rolled back otherwise.
// Panics within fn cause rollback and re-panic.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback(ctx)
			panic(r)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			return fmt.Errorf("rollback transaction: %w (original: %v)", rbErr, err)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

func applyDefaults(poolCfg *pgxpool.Config, cfg Config) {
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	} else {
		poolCfg.MaxConns = 10
	}

	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	} else {
		poolCfg.MinConns = 2
	}

	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	} else {
		poolCfg.MaxConnLifetime = time.Hour
	}

	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	} else {
		poolCfg.MaxConnIdleTime = 30 * time.Minute
	}

	if cfg.HealthCheckPeriod > 0 {
		poolCfg.HealthCheckPeriod = cfg.HealthCheckPeriod
	} else {
		poolCfg.HealthCheckPeriod = time.Minute
	}
}
