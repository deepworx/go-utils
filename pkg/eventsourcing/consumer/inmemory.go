package consumer

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/deepworx/go-utils/internal/storehook"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/postgres"
)

// InMemoryRuntimeConfig configures an InMemoryRuntime.
type InMemoryRuntimeConfig struct {
	// Store is the event log the Consumers read.
	Store *eventstore.InMemoryStore
}

// InMemoryRuntime runs Consumers on an eventstore.InMemoryStore, for
// prototypes and tests. Cursors live in memory. A Cursor transaction is a
// postgres.InMemoryUnitOfWork transaction, so handler writes registered with
// postgres.Stage commit or roll back together with the Cursor.
type InMemoryRuntime struct {
	feed memFeed
	uow  *postgres.InMemoryUnitOfWork

	mu      sync.Mutex
	cursors map[string]*memCursor
}

// memFeed is the read side of eventstore.InMemoryStore, reached through
// internal/storehook.
type memFeed interface {
	Codec() *eventstore.Codec
	Changed() <-chan struct{}
	Next(after eventstore.Cursor, types []string) (eventstore.Envelope, eventstore.Payload, bool)
	Head(types []string) eventstore.Cursor
	Horizon() uint64
}

type memCursor struct {
	cursor  eventstore.Cursor
	stopped *Stop
	// claimed reports that the Cursor was claimed once; eventTypes is the
	// filter accepted then.
	claimed    bool
	eventTypes []string
	attached   bool
	held       bool
	resumed    chan struct{}
}

// NewInMemoryRuntime creates an InMemoryRuntime over cfg.Store. It panics
// on a nil Store.
func NewInMemoryRuntime(cfg InMemoryRuntimeConfig) *InMemoryRuntime {
	f, ok := storehook.InMemoryFeed(cfg.Store)
	if !ok {
		panic("consumer: NewInMemoryRuntime: nil store")
	}
	return &InMemoryRuntime{
		feed:    f.(memFeed),
		uow:     postgres.NewInMemoryUnitOfWork(),
		cursors: make(map[string]*memCursor),
	}
}

// Resume clears the Stopped state of the named Consumer.
func (rt *InMemoryRuntime) Resume(_ context.Context, name string) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	c, ok := rt.cursors[name]
	if !ok {
		return fmt.Errorf("resume %q: %w", name, ErrNotFound)
	}
	if c.stopped == nil {
		return nil
	}
	c.stopped = nil
	select {
	case c.resumed <- struct{}{}:
	default:
	}
	return nil
}

func (rt *InMemoryRuntime) attach(in attachInput) (port, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	c, ok := rt.cursors[in.name]
	switch {
	case !ok:
		c = &memCursor{resumed: make(chan struct{}, 1)}
		rt.cursors[in.name] = c
	case c.attached:
		return nil, fmt.Errorf("%q: %w", in.name, ErrDuplicate)
	}
	c.attached = true
	return &memPort{rt: rt, types: in.eventTypes, mode: in.mode, cursor: c}, nil
}

// restart returns a new Runtime over the same Store with a copy of the
// Cursors, none of them attached.
func (rt *InMemoryRuntime) restart() *InMemoryRuntime {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	next := &InMemoryRuntime{
		feed:    rt.feed,
		uow:     postgres.NewInMemoryUnitOfWork(),
		cursors: make(map[string]*memCursor, len(rt.cursors)),
	}
	for name, c := range rt.cursors {
		next.cursors[name] = &memCursor{
			cursor:     c.cursor,
			stopped:    c.stopped,
			claimed:    c.claimed,
			eventTypes: c.eventTypes,
			resumed:    make(chan struct{}, 1),
		}
	}
	return next
}

type memPort struct {
	rt     *InMemoryRuntime
	types  []string
	mode   Mode
	cursor *memCursor
}

func (p *memPort) codec() *eventstore.Codec { return p.rt.feed.Codec() }

func (p *memPort) acquire(ctx context.Context) (context.Context, func(), error) {
	p.rt.mu.Lock()
	defer p.rt.mu.Unlock()
	if p.cursor.held {
		return nil, nil, ErrRunning
	}
	p.cursor.held = true
	p.claimFilter()
	var once sync.Once
	release := func() {
		once.Do(func() {
			p.rt.mu.Lock()
			defer p.rt.mu.Unlock()
			p.cursor.held = false
		})
	}
	return ctx, release, nil
}

// claimFilter records the configured filter on the Cursor, or stops the
// Consumer when the change would leave a gap. A Stopped Cursor is checked
// again once resumed. The caller holds rt.mu.
func (p *memPort) claimFilter() {
	c := p.cursor
	if c.stopped != nil || (c.claimed && slices.Equal(c.eventTypes, p.types)) {
		return
	}
	if change := diffFilter(c.eventTypes, p.types); c.claimed && change.needsHistoryCheck(p.mode) &&
		p.hasHistory(change, c.cursor) {
		c.stopped = &Stop{At: time.Now(), Cause: change.cause()}
		return
	}
	c.claimed = true
	c.eventTypes = p.types
}

// hasHistory reports whether an event of a type added by f lies at or below
// upTo.
func (p *memPort) hasHistory(f filterChange, upTo eventstore.Cursor) bool {
	if !f.widened {
		env, _, ok := p.rt.feed.Next(eventstore.Cursor{}, f.added)
		return ok && env.Cursor.Compare(upTo) <= 0
	}
	for after := (eventstore.Cursor{}); ; {
		env, _, ok := p.rt.feed.Next(after, nil)
		if !ok || env.Cursor.Compare(upTo) > 0 {
			return false
		}
		if !slices.Contains(f.stored, env.EventType) {
			return true
		}
		after = env.Cursor
	}
}

func (p *memPort) load(context.Context) (cursorState, error) {
	p.rt.mu.Lock()
	defer p.rt.mu.Unlock()
	if p.cursor.held {
		p.claimFilter()
	}
	state := cursorState{cursor: p.cursor.cursor}
	if s := p.cursor.stopped; s != nil {
		stopped := *s
		state.stopped = &stopped
	}
	return state, nil
}

func (p *memPort) read(_ context.Context, in readInput) ([]record, error) {
	var records []record
	after := in.after
	for len(records) < in.limit {
		env, payload, ok := p.rt.feed.Next(after, p.types)
		if !ok {
			break
		}
		records = append(records, record{env: env, payload: payload})
		after = env.Cursor
	}
	return records, nil
}

func (p *memPort) head(context.Context) (eventstore.Cursor, error) {
	return p.rt.feed.Head(p.types), nil
}

func (p *memPort) status(ctx context.Context) (Status, error) {
	state, err := p.load(ctx)
	if err != nil {
		return Status{}, err
	}
	s := Status{Cursor: state.cursor, Stopped: state.stopped}
	after := state.cursor
	for s.Pending <= PendingCap {
		env, _, ok := p.rt.feed.Next(after, p.types)
		if !ok {
			break
		}
		if s.Pending == 0 {
			s.Age = max(time.Since(env.RecordedAt), 0)
		}
		s.Pending++
		after = env.Cursor
	}
	return s, nil
}

// frontier takes the horizon before the next event: a transaction that
// starts in between gets a larger ID, so every event at or below a
// position under the horizon is already visible to Next.
func (p *memPort) frontier(_ context.Context, after eventstore.Cursor) (frontier, error) {
	f := frontier{xmin: p.rt.feed.Horizon()}
	if env, _, ok := p.rt.feed.Next(after, p.types); ok {
		f.next = env.Cursor
	}
	return f, nil
}

func (p *memPort) transact(ctx context.Context, fn func(ctx context.Context) error) error {
	return p.rt.uow.Execute(ctx, fn)
}

func (p *memPort) advance(ctx context.Context, m move) error {
	postgres.Stage(ctx, func() {
		p.rt.mu.Lock()
		defer p.rt.mu.Unlock()
		if p.cursor.cursor == m.from {
			p.cursor.cursor = m.to
		}
	})
	return nil
}

func (p *memPort) stop(_ context.Context, s Stop) error {
	p.rt.mu.Lock()
	defer p.rt.mu.Unlock()
	p.cursor.stopped = &s
	return nil
}

func (p *memPort) changed() <-chan struct{} { return p.rt.feed.Changed() }

func (p *memPort) resumed() <-chan struct{} { return p.cursor.resumed }

var _ Runtime = (*InMemoryRuntime)(nil)
