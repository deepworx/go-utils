// PROTOTYPE (DWX-221): throwaway sketch of pkg/eventsourcing/reactor.
//
// A Reactor is a Consumer in OwnTransaction mode with a park loop around its
// Handler. It issues commands through the app's own Repository; the library
// only stamps the context (tenant, actor, causation, correlation).
//
// Failure ladder, per event:
//
//	handler error ──retry (backoff)──▶ attempts spent ──▶ park (tenant, stream)
//	Permanent(err) / panic / decode ──────────────────▶ park at once
//	park list unwritable ─────────────────────────────▶ Consumer Stopped
//
// A parked stream holds every later event of that stream for this Reactor, so
// the stream's order survives while the Cursor moves on for everyone else.
package reactor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/eventstore"
)

var (
	ErrStartAtRequired = errors.New("reactor: StartAt is required")
	ErrNotParked       = errors.New("reactor: stream not parked")
)

// StartAt says where a Reactor begins when it has no Cursor yet. There is no
// default: replaying history into side effects must be a conscious choice.
type StartAt int

const (
	startAtUnset StartAt = iota
	// FromNow skips the history before the Reactor's first claim.
	FromNow
	// FromBeginning replays the whole log; right when every reaction is a
	// no-op on aggregates that already reacted.
	FromBeginning
)

// Config is a Reactor's identity and semantics.
type Config struct {
	Name       string           // permanent: Cursor key, park key, metric label
	Handler    consumer.Handler // usually a *consumer.Router
	EventTypes []string         // only for a raw Handler
	StartAt    StartAt
}

// Option tunes a Reactor.
type Option func(*options)

type options struct {
	attempts int
	backoff  time.Duration
	timeout  time.Duration
}

// WithRetry sets attempts per event before parking (real: consumer.Retry).
func WithRetry(attempts int, base time.Duration) Option {
	return func(o *options) { o.attempts, o.backoff = attempts, base }
}

// WithTimeout bounds one reaction, outbound calls included (default 60s).
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// Parked is one held event.
type Parked struct {
	Tenant         string
	StreamID       uuid.UUID
	StreamType     string
	StreamPosition int
	EventID        uuid.UUID
	EventType      string
	Cause          string // empty for events queued behind the head
	Attempts       int
	ParkedAt       time.Time
}

// Stream names a stream across tenants (stream identity is per tenant, ADR-0002).
type Stream struct {
	Tenant string
	ID     uuid.UUID
}

// Reactor is a running reactor definition.
type Reactor struct {
	rt   *consumer.InMemoryRuntime
	c    *consumer.Consumer
	cfg  Config
	opts options

	// real: table parked_events (consumer, tenant_id, stream_id, event_id, ...)
	// written by the Runtime's pool; the hold check and Redrive lock one hold
	// row per (consumer, tenant, stream) so they cannot interleave.
	mu    sync.Mutex
	parks map[Stream][]Parked
}

// New builds the Reactor's Consumer on rt. Add r.Consumer() to a consumer.Group.
func New(rt *consumer.InMemoryRuntime, cfg Config, opts ...Option) (*Reactor, error) {
	if cfg.StartAt == startAtUnset {
		return nil, fmt.Errorf("%q: %w", cfg.Name, ErrStartAtRequired)
	}
	o := options{attempts: 4, backoff: 500 * time.Millisecond, timeout: 60 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	if typed, ok := cfg.Handler.(interface{ EventTypes() []string }); ok && len(cfg.EventTypes) == 0 {
		cfg.EventTypes = typed.EventTypes() // ADR-0006: filter derived from the Router
	}
	r := &Reactor{rt: rt, cfg: cfg, opts: o, parks: map[Stream][]Parked{}}
	c, err := consumer.New(rt, consumer.Config{
		Name:       cfg.Name,
		EventTypes: cfg.EventTypes,
		Mode:       consumer.OwnTransaction, // never a parameter: no tx held across HTTP
		Handler:    consumer.HandlerFunc(r.handle),
		OnFirstClaim: func(head eventstore.Cursor) eventstore.Cursor {
			if cfg.StartAt == FromNow {
				return head
			}
			return eventstore.Cursor{}
		},
	})
	if err != nil {
		return nil, err
	}
	r.c = c
	return r, nil
}

func (r *Reactor) Consumer() *consumer.Consumer { return r.c }

// Drain is consumer.Drain (tests, prototypes).
func (r *Reactor) Drain(ctx context.Context) error { return r.c.Drain(ctx) }

// Status adds the park list to the Consumer's Status.
type Status struct {
	consumer.Status
	ParkedStreams int
	ParkedEvents  int
}

func (r *Reactor) Status(ctx context.Context) (Status, error) {
	s, err := r.c.Status(ctx)
	if err != nil {
		return Status{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Status{Status: s, ParkedStreams: len(r.parks)}
	for _, held := range r.parks {
		st.ParkedEvents += len(held)
	}
	return st, nil
}

// Parked lists held events, oldest hold first, in replay order within a
// stream. An empty tenant lists every tenant (the Ops view).
func (r *Reactor) Parked(_ context.Context, tenant string) ([]Parked, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Parked
	for s, held := range r.parks {
		if tenant == "" || s.Tenant == tenant {
			out = append(out, held...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StreamID != out[j].StreamID {
			return out[i].ParkedAt.Before(out[j].ParkedAt)
		}
		return out[i].StreamPosition < out[j].StreamPosition
	})
	return out, nil
}

// RedriveResult is how far a Redrive got.
type RedriveResult struct {
	Replayed  []Parked
	Remaining int
}

// Redrive replays a parked stream in order, one attempt per event, releasing
// each success. It stops at the first failure and leaves the rest held.
// It loops until the hold is empty, so events the live Consumer queued during
// the redrive are replayed too. The Cursor is never touched.
func (r *Reactor) Redrive(ctx context.Context, s Stream) (RedriveResult, error) {
	var res RedriveResult
	for {
		r.mu.Lock()
		held := r.parks[s]
		if len(held) == 0 {
			delete(r.parks, s)
			r.mu.Unlock()
			if len(res.Replayed) == 0 {
				return res, fmt.Errorf("%s/%s: %w", s.Tenant, s.ID, ErrNotParked)
			}
			return res, nil
		}
		head := held[0]
		r.mu.Unlock()

		ev, ok := r.rt.Store.ReadOne(s.Tenant, head.EventID)
		if !ok {
			return res, fmt.Errorf("redrive: event %s vanished", head.EventID)
		}
		if err := r.react(eventstore.WithTenant(ctx, s.Tenant), ev); err != nil {
			r.mu.Lock()
			r.parks[s][0].Cause = err.Error()
			r.parks[s][0].Attempts++
			res.Remaining = len(r.parks[s])
			r.mu.Unlock()
			return res, fmt.Errorf("redrive %s@%d: %w (stream stays parked)", head.EventType, head.StreamPosition, err)
		}
		r.mu.Lock()
		r.parks[s] = r.parks[s][1:]
		r.mu.Unlock()
		res.Replayed = append(res.Replayed, head)
	}
}

// Discard drops one parked event without reacting: the operator's decision
// that it will never succeed. If it was the head, the next Redrive starts
// behind it.
func (r *Reactor) Discard(_ context.Context, s Stream, event uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	held := r.parks[s]
	for i, p := range held {
		if p.EventID == event {
			r.parks[s] = append(held[:i:i], held[i+1:]...)
			if len(r.parks[s]) == 0 {
				delete(r.parks, s)
			}
			return nil
		}
	}
	return fmt.Errorf("%s/%s event %s: %w", s.Tenant, s.ID, event, ErrNotParked)
}

// --- the park loop -------------------------------------------------------------

func (r *Reactor) handle(ctx context.Context, env eventstore.Envelope) error {
	s := Stream{Tenant: env.TenantID, ID: env.StreamID}

	r.mu.Lock()
	_, held := r.parks[s]
	r.mu.Unlock()
	if held {
		return r.park(s, env, "", 0) // queued behind the head
	}

	var err error
	for attempt := 1; attempt <= r.opts.attempts; attempt++ {
		if err = r.react(ctx, env); err == nil {
			return nil
		}
		if consumer.IsPermanent(err) {
			return r.park(s, env, err.Error(), attempt)
		}
		if attempt < r.opts.attempts {
			select {
			case <-time.After(r.opts.backoff << (attempt - 1)):
			case <-ctx.Done():
				return ctx.Err() // shutdown: the event repeats (OwnTransaction)
			}
		}
	}
	return r.park(s, env, err.Error(), r.opts.attempts)
}

// react stamps the context every command of this reaction carries and turns
// a panic into a Permanent failure.
func (r *Reactor) react(ctx context.Context, env eventstore.Envelope) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = consumer.Permanent(fmt.Errorf("reactor %q panicked: %v", r.cfg.Name, p))
		}
	}()
	ctx = eventstore.WithActor(ctx, "system:"+r.cfg.Name)
	ctx = eventstore.WithCausation(ctx, env.EventID.String()[28:])
	ctx = eventstore.WithCorrelation(ctx, env.CorrelationID)
	ctx, cancel := context.WithTimeout(ctx, r.opts.timeout)
	defer cancel()
	return r.cfg.Handler.Handle(ctx, env)
}

// park returns nil so the Cursor moves on. Real: a failing INSERT is
// returned, and the core retries and finally Stops the Consumer, because
// advancing past an unrecorded event would lose it.
func (r *Reactor) park(s Stream, env eventstore.Envelope, cause string, attempts int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.parks[s] {
		if p.EventID == env.EventID {
			return nil // OwnTransaction repeat after a crash: parking twice is free
		}
	}
	r.parks[s] = append(r.parks[s], Parked{
		Tenant: s.Tenant, StreamID: s.ID, StreamType: env.StreamType,
		StreamPosition: env.StreamPosition, EventID: env.EventID, EventType: env.EventType,
		Cause: cause, Attempts: attempts, ParkedAt: time.Now(),
	})
	return nil
}
