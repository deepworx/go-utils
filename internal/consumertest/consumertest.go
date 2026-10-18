// Package consumertest is the contract suite that every consumer Runtime (Pg
// and in-memory) must pass. A runtime test calls
//
//	consumertest.Run(t, func(t *testing.T, codec *eventstore.Codec) consumertest.Backend { ... })
//
// and the suite exercises it only through the public consumer API.
package consumertest

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/postgres"
)

// Effects is a read model a handler writes to. Record joins the Cursor
// transaction in ctx, so a rolled-back transaction drops its records.
type Effects interface {
	Record(ctx context.Context, key string) error
	// Keys returns the committed keys in record order.
	Keys(ctx context.Context) ([]string, error)
}

// Backend is one empty event log with a Runtime over it.
type Backend struct {
	// Store and UnitOfWork write events; Store encodes with the suite's
	// codec.
	Store      eventstore.Store
	UnitOfWork eventstore.UnitOfWork
	Runtime    consumer.Runtime
	Effects    Effects
	// Restart returns a new Runtime over the same log and Cursors, as after
	// a process restart. Runtimes it returned earlier are no longer used.
	Restart func(t *testing.T) consumer.Runtime
}

// Option configures Run.
type Option func(*config)

type config struct {
	synctest   bool
	sequential bool
}

// Sequential runs the cases one after another. For Pg backends whose
// databases share one cluster: pg_snapshot_xmin is cluster-wide, so a
// transaction of a parallel case would hide fresh events from Drain.
func Sequential() Option {
	return func(c *config) { c.sequential = true }
}

// InBubble runs every case and its newBackend inside a testing/synctest
// bubble, so retries and Age use fake time. For in-memory backends.
func InBubble() Option {
	return func(c *config) { c.synctest = true }
}

// Run runs the contract suite. newBackend is called once per case with the
// codec the Store must encode with.
func Run(t *testing.T, newBackend func(t *testing.T, codec *eventstore.Codec) Backend, opts ...Option) {
	t.Helper()
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}
	s := suite{newBackend: newBackend, synctest: cfg.synctest}
	for _, tt := range append(cases(s), filterChangeCases(s)...) {
		t.Run(tt.name, func(t *testing.T) {
			if !cfg.sequential {
				t.Parallel()
			}
			if cfg.synctest {
				synctest.Test(t, tt.fn)
				return
			}
			tt.fn(t)
		})
	}
}

// InMemoryEffects is Effects for in-memory backends; Record stages its
// write with postgres.Stage.
type InMemoryEffects struct {
	mu   sync.Mutex
	keys []string
}

// Record stages key.
func (e *InMemoryEffects) Record(ctx context.Context, key string) error {
	postgres.Stage(ctx, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.keys = append(e.keys, key)
	})
	return nil
}

// Keys returns the committed keys.
func (e *InMemoryEffects) Keys(context.Context) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.keys), nil
}

// PgEffects is Effects for Pg backends. Record inserts through
// postgres.Conn, so it joins the Cursor transaction in ctx.
type PgEffects struct {
	pool *pgxpool.Pool
}

// NewPgEffects creates the effects table in pool's database.
func NewPgEffects(t *testing.T, pool *pgxpool.Pool) *PgEffects {
	t.Helper()
	_, err := pool.Exec(t.Context(), `CREATE TABLE consumertest_effects (
		seq BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		key TEXT NOT NULL)`)
	if err != nil {
		t.Fatalf("create effects table: %v", err)
	}
	return &PgEffects{pool: pool}
}

// Record inserts key.
func (e *PgEffects) Record(ctx context.Context, key string) error {
	_, err := postgres.Conn(ctx, e.pool).Exec(ctx, `INSERT INTO consumertest_effects (key) VALUES ($1)`, key)
	return err
}

// Keys returns the committed keys.
func (e *PgEffects) Keys(ctx context.Context) ([]string, error) {
	rows, err := e.pool.Query(ctx, `SELECT key FROM consumertest_effects ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
