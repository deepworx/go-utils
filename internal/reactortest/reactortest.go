// Package reactortest is the contract suite that every Reactor backend (Pg
// and in-memory consumer Runtimes) must pass. A test calls
//
//	reactortest.Run(t, func(t *testing.T, codec *eventstore.Codec) reactortest.Backend { ... })
//
// and the suite exercises the Reactor only through its public API.
package reactortest

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/reactor"
)

// Backend is one empty event log with a consumer Runtime over it.
type Backend struct {
	// Store and UnitOfWork write events; Store encodes with the suite's
	// codec.
	Store      eventstore.Store
	UnitOfWork eventstore.UnitOfWork
	Runtime    consumer.Runtime
	// Redeliver hands env to r's live handler once more, as after a crash
	// between handling an event and advancing the Cursor.
	Redeliver func(ctx context.Context, r *reactor.Reactor, env eventstore.Envelope) error
	// Discarded lists the audit rows Discard kept for the named Reactor.
	// Nil for a backend that keeps none.
	Discarded func(ctx context.Context, reactorName string) ([]Discarded, error)
}

// Discarded is the audit record of a discarded event.
type Discarded struct {
	EventID uuid.UUID
	By      string
}

// Option configures Run.
type Option func(*config)

type config struct {
	synctest   bool
	sequential bool
}

// Sequential runs the cases one after another, for Pg backends whose
// databases share one cluster (pg_snapshot_xmin is cluster-wide).
func Sequential() Option {
	return func(c *config) { c.sequential = true }
}

// InBubble runs every case inside a testing/synctest bubble, so retries and
// timeouts use fake time. For in-memory backends.
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
	s := suite{newBackend: newBackend}
	for _, tt := range cases(s) {
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
