// PROTOTYPE (DWX-221): stand-in for the DWX-220 consumer API, only what a
// Reactor needs: Runtime, New, Router/On, Permanent, Drain, Status, Stopped.
// No leases, no polling loop, no retries in the core (Reactors bring their own).
package consumer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/eventstore"
)

var (
	ErrDuplicate = errors.New("consumer: duplicate name")
	ErrStopped   = errors.New("consumer: stopped")
)

// Handler handles one event. Tenant is already set in ctx by the runner.
type Handler interface {
	Handle(ctx context.Context, env eventstore.Envelope) error
}

// HandlerFunc adapts a func.
type HandlerFunc func(ctx context.Context, env eventstore.Envelope) error

func (f HandlerFunc) Handle(ctx context.Context, env eventstore.Envelope) error { return f(ctx, env) }

// --- Permanent ---------------------------------------------------------------

type permanentError struct{ err error }

func (p permanentError) Error() string { return "permanent: " + p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

// Permanent marks err as not worth retrying. A Consumer goes Stopped at once;
// a Reactor parks the stream at once.
func Permanent(err error) error { return permanentError{err} }

// IsPermanent reports whether err was marked Permanent.
func IsPermanent(err error) bool { return errors.As(err, new(permanentError)) }

// --- Router ------------------------------------------------------------------

// Router dispatches by event type, decoding through the Codec.
type Router struct {
	store  *eventstore.InMemoryStore
	routes map[string]func(ctx context.Context, env eventstore.Envelope, ev any) error
	types  []string
}

func NewRouter(store *eventstore.InMemoryStore) *Router {
	return &Router{store: store, routes: map[string]func(context.Context, eventstore.Envelope, any) error{}}
}

// On registers fn for event type E.
func On[E interface{ EventType() string }](r *Router, fn func(ctx context.Context, env eventstore.Envelope, e E) error) {
	var zero E
	r.types = append(r.types, zero.EventType())
	r.routes[zero.EventType()] = func(ctx context.Context, env eventstore.Envelope, ev any) error {
		return fn(ctx, env, ev.(E))
	}
}

// EventTypes is the filter derived from the routes (ADR-0006).
func (r *Router) EventTypes() []string { return r.types }

func (r *Router) Handle(ctx context.Context, env eventstore.Envelope) error {
	route, ok := r.routes[env.EventType]
	if !ok {
		return nil
	}
	ev, err := r.store.Decode(env.EventType, env.Data)
	if err != nil {
		return Permanent(fmt.Errorf("decode: %w", err))
	}
	return route(ctx, env, ev)
}

// --- Runtime + Consumer ------------------------------------------------------

// InMemoryRuntime holds Cursors and names.
type InMemoryRuntime struct {
	Store *eventstore.InMemoryStore

	mu      sync.Mutex
	cursors map[string]*cursorRow
}

type cursorRow struct {
	at      eventstore.Cursor
	seen    bool // a row exists (even at zero): used by StartAt seeding
	stopped error
}

func NewInMemoryRuntime(store *eventstore.InMemoryStore) *InMemoryRuntime {
	return &InMemoryRuntime{Store: store, cursors: map[string]*cursorRow{}}
}

// Mode picks the transaction shape.
type Mode int

const (
	Transactional Mode = iota
	OwnTransaction
)

// Config is a Consumer's identity and semantics.
type Config struct {
	Name       string
	EventTypes []string
	Mode       Mode
	Handler    Handler
	// OnFirstClaim runs once when no Cursor row exists yet; it may return a
	// starting Cursor. Internal hook for reactor.StartAt (not public API).
	OnFirstClaim func(head eventstore.Cursor) eventstore.Cursor
}

// Consumer is one named reader.
type Consumer struct {
	rt  *InMemoryRuntime
	cfg Config
}

func New(rt *InMemoryRuntime, cfg Config) (*Consumer, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if _, ok := rt.cursors[cfg.Name]; ok {
		return nil, fmt.Errorf("%q: %w", cfg.Name, ErrDuplicate)
	}
	if r, ok := cfg.Handler.(*Router); ok && len(cfg.EventTypes) == 0 {
		cfg.EventTypes = r.EventTypes()
	}
	rt.cursors[cfg.Name] = &cursorRow{}
	return &Consumer{rt: rt, cfg: cfg}, nil
}

func (c *Consumer) Name() string { return c.cfg.Name }

// Drain processes synchronously up to the head at call time.
func (c *Consumer) Drain(ctx context.Context) error {
	c.rt.mu.Lock()
	row := c.rt.cursors[c.cfg.Name]
	if !row.seen {
		row.seen = true
		if c.cfg.OnFirstClaim != nil {
			row.at = c.cfg.OnFirstClaim(c.rt.Store.Head())
		}
	}
	c.rt.mu.Unlock()

	if row.stopped != nil {
		return fmt.Errorf("%s: %w: %v", c.cfg.Name, ErrStopped, row.stopped)
	}
	for _, env := range c.rt.Store.ReadAfter(row.at, c.cfg.EventTypes) {
		hctx := eventstore.WithTenant(ctx, env.TenantID)
		if err := c.cfg.Handler.Handle(hctx, env); err != nil {
			row.stopped = err // real: retries first (WithRetry), then Stopped
			return fmt.Errorf("%s: %w: %v", c.cfg.Name, ErrStopped, err)
		}
		row.at = env.Cursor // OwnTransaction: advance after each event
	}
	return nil
}

// Status is a trimmed DWX-220 Status.
type Status struct {
	Cursor  eventstore.Cursor
	Pending int
	Stopped error
}

func (c *Consumer) Status(context.Context) (Status, error) {
	c.rt.mu.Lock()
	row := c.rt.cursors[c.cfg.Name]
	c.rt.mu.Unlock()
	return Status{
		Cursor:  row.at,
		Pending: len(c.rt.Store.ReadAfter(row.at, c.cfg.EventTypes)),
		Stopped: row.stopped,
	}, nil
}

// Rewind is a test hook: pretend the Cursor write after an event was lost
// (crash between the reaction's commit and the Cursor's commit).
func (c *Consumer) Rewind(to eventstore.Cursor) {
	c.rt.mu.Lock()
	defer c.rt.mu.Unlock()
	c.rt.cursors[c.cfg.Name].at = to
}
