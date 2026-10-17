package consumer

import (
	"context"
	"fmt"
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
	TraceContext(at eventstore.Cursor) string
}

type memCursor struct {
	cursor  eventstore.Cursor
	stopped *Stop
	held    bool
	resumed chan struct{}
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
	if _, ok := rt.cursors[in.name]; ok {
		return nil, fmt.Errorf("%q: %w", in.name, ErrDuplicate)
	}
	c := &memCursor{resumed: make(chan struct{}, 1)}
	rt.cursors[in.name] = c
	return &memPort{rt: rt, types: in.eventTypes, cursor: c}, nil
}

type memPort struct {
	rt     *InMemoryRuntime
	types  []string
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

func (p *memPort) load(context.Context) (cursorState, error) {
	p.rt.mu.Lock()
	defer p.rt.mu.Unlock()
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
		records = append(records, record{env: env, payload: payload, traceContext: p.rt.feed.TraceContext(env.Cursor)})
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
