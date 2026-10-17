package consumer

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/internal/projectionhook"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

// This file serves package projection through internal/projectionhook: the
// change channel of each Runtime and resetting Consumers.

func init() {
	projectionhook.Register(changesOf, reset)
}

func changesOf(rt any) (projectionhook.Changes, bool) {
	switch rt := rt.(type) {
	case *InMemoryRuntime:
		if rt == nil {
			return nil, false
		}
		return rt.changes, true
	case *PgRuntime:
		if rt == nil {
			return nil, false
		}
		return &pgChanges{
			pool:     rt.pool,
			listener: rt.listener,
			channel:  rt.schema + ".changes",
		}, true
	}
	return nil, false
}

// rewinder moves a Cursor back to the start of the log, clears Stopped and
// accepts the configured filter. Inside a Cursor transaction it commits
// with it.
type rewinder interface {
	rewind(ctx context.Context) error
}

// reset rewinds in.Consumers after in.Clear in one Cursor transaction, while
// holding their leases.
func reset(ctx context.Context, in projectionhook.ResetInput) error {
	if len(in.Consumers) == 0 {
		return fmt.Errorf("reset: no consumers: %w", ErrInvalidConfig)
	}
	consumers := make([]*Consumer, len(in.Consumers))
	for i, c := range in.Consumers {
		cons, ok := c.(*Consumer)
		if !ok || cons == nil {
			return fmt.Errorf("reset: %T is no consumer: %w", c, ErrInvalidConfig)
		}
		if cons.running.Load() {
			return fmt.Errorf("reset %q: %w", cons.name, ErrRunning)
		}
		consumers[i] = cons
	}
	for _, c := range consumers {
		leaseCtx, release, err := c.port.acquire(ctx)
		if err != nil {
			return fmt.Errorf("reset %q: %w", c.name, err)
		}
		defer release()
		ctx = leaseCtx
	}
	return consumers[0].port.transact(ctx, func(ctx context.Context) error {
		if err := in.Clear(ctx); err != nil {
			return fmt.Errorf("reset: clear: %w", err)
		}
		for _, c := range consumers {
			r, ok := c.port.(rewinder)
			if !ok {
				return fmt.Errorf("reset %q: runtime cannot rewind: %w", c.name, ErrInvalidConfig)
			}
			if err := r.rewind(ctx); err != nil {
				return fmt.Errorf("reset %q: rewind: %w", c.name, err)
			}
		}
		return nil
	})
}

func (p *memPort) rewind(ctx context.Context) error {
	postgres.Stage(ctx, func() {
		p.rt.mu.Lock()
		defer p.rt.mu.Unlock()
		p.cursor.cursor = eventstore.Cursor{}
		p.cursor.stopped = nil
		p.cursor.claimed = true
		p.cursor.eventTypes = p.types
	})
	return nil
}

func (p *pgPort) rewind(ctx context.Context) error {
	cursors := pgx.Identifier{p.rt.schema, "consumer_cursors"}.Sanitize()
	_, err := postgres.Conn(ctx, p.rt.pool).Exec(ctx, `UPDATE `+cursors+`
		SET transaction_id = '0', global_position = 0, event_types = $2,
			stopped = false, stopped_at = NULL, stopped_error = NULL
		WHERE name = $1`, p.name, p.types)
	return err
}

// changeBuffer is the capacity of an in-memory change Feed, the default of
// notify.Listener.
const changeBuffer = 64

// memChanges delivers change payloads in process.
type memChanges struct {
	mu    sync.Mutex
	feeds map[*memChangeFeed]struct{}
}

func newMemChanges() *memChanges {
	return &memChanges{feeds: make(map[*memChangeFeed]struct{})}
}

// Publish stages the delivery with the in-memory transaction in ctx.
func (m *memChanges) Publish(ctx context.Context, payload string) error {
	postgres.Stage(ctx, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for f := range m.feeds {
			deliver(f.ch, notify.Notification{Payload: payload})
		}
	})
	return nil
}

func (m *memChanges) Subscribe() (projectionhook.Feed, error) {
	f := &memChangeFeed{changes: m, ch: make(chan notify.Notification, changeBuffer)}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.feeds[f] = struct{}{}
	return f, nil
}

// deliver sends n without blocking. A full buffer is drained and replaced by
// a single Gap, like notify.Listener does.
func deliver(ch chan notify.Notification, n notify.Notification) {
	select {
	case ch <- n:
		return
	default:
	}
	for drained := false; !drained; {
		select {
		case <-ch:
		default:
			drained = true
		}
	}
	ch <- notify.Notification{Gap: true}
}

type memChangeFeed struct {
	changes *memChanges
	ch      chan notify.Notification
}

func (f *memChangeFeed) C() <-chan notify.Notification { return f.ch }

func (f *memChangeFeed) Close() {
	f.changes.mu.Lock()
	defer f.changes.mu.Unlock()
	if _, ok := f.changes.feeds[f]; !ok {
		return
	}
	delete(f.changes.feeds, f)
	close(f.ch)
}

// pgChanges sends change payloads with pg_notify, which Postgres delivers
// when the sending transaction commits.
type pgChanges struct {
	pool     *pgxpool.Pool
	listener *notify.Listener
	channel  string
}

func (c *pgChanges) Publish(ctx context.Context, payload string) error {
	_, err := postgres.Conn(ctx, c.pool).Exec(ctx, `SELECT pg_notify($1, $2)`, c.channel, payload)
	return err
}

func (c *pgChanges) Subscribe() (projectionhook.Feed, error) {
	if c.listener == nil {
		return nil, fmt.Errorf("subscribe %s: the PgRuntime has no Listener: %w", c.channel, ErrInvalidConfig)
	}
	sub, err := c.listener.Subscribe(c.channel)
	if err != nil {
		return nil, fmt.Errorf("subscribe %s: %w", c.channel, err)
	}
	return sub, nil
}
