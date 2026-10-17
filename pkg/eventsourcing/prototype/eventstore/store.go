// PROTOTYPE (DWX-212): throwaway API sketch, not production code.
//
// Only the in-memory side exists; it fakes just enough of DWX-209/210/211
// (command transaction, codec, tenant) to drive the repository API.
package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/aggregate"
)

var (
	ErrNotFound              = errors.New("eventstore: stream not found")
	ErrAlreadyExists         = errors.New("eventstore: stream already exists")
	ErrConcurrencyConflict   = errors.New("eventstore: concurrency conflict")
	ErrNotCommandTransaction = errors.New("eventstore: not in a command transaction")
	ErrStreamMismatch        = errors.New("eventstore: command transaction holds another stream")
	ErrNestedUnitOfWork      = errors.New("eventstore: nested command transaction")
	ErrNoTenant              = errors.New("eventstore: no tenant in context")
)

// Cursor is a position in the global log (xid8, global_position).
type Cursor struct {
	TransactionID uint64
	Position      int64
}

// CommandResult is what a command returns: where to wait (read-your-writes
// via consumer.Gate) and the version the caller's next command expects.
// Zero Position = nothing written (idempotent repeat).
type CommandResult struct {
	Position Cursor
	Version  int
}

// --- tenant (stand-in for ctxutil.WithTenantID) -----------------------------

type tenantKey struct{}

func WithTenant(ctx context.Context, t string) context.Context {
	return context.WithValue(ctx, tenantKey{}, t)
}

func tenant(ctx context.Context) (string, error) {
	t, ok := ctx.Value(tenantKey{}).(string)
	if !ok || t == "" {
		return "", ErrNoTenant
	}
	return t, nil
}

// --- codec (stand-in for DWX-210) -------------------------------------------

type Codec struct{ types map[string]reflect.Type }

func NewCodec() *Codec { return &Codec{types: map[string]reflect.Type{}} }

func Register[E aggregate.Event](c *Codec) {
	var e E
	c.types[e.EventType()] = reflect.TypeOf(e)
}

func (c *Codec) decode(eventType string, data []byte) (aggregate.Event, error) {
	t, ok := c.types[eventType]
	if !ok {
		return nil, fmt.Errorf("decode %s: unregistered event type", eventType)
	}
	v := reflect.New(t)
	if err := json.Unmarshal(data, v.Interface()); err != nil {
		return nil, fmt.Errorf("decode %s: %w", eventType, err)
	}
	return v.Elem().Interface().(aggregate.Event), nil
}

// --- store ------------------------------------------------------------------

// Envelope is one stored event (trimmed).
type Envelope struct {
	Cursor         Cursor
	TenantID       string
	StreamID       uuid.UUID
	StreamType     string
	StreamPosition int
	EventType      string
	Data           []byte
	OccurredOn     time.Time

	// Added for DWX-221: log metadata the Reactor reads and stamps.
	EventID       uuid.UUID
	Actor         string
	CorrelationID string
	CausationID   string
}

// --- metadata (stand-in for ctxutil.WithUserID/WithCorrelationID/WithCausationID)

type metaKey struct{ name string }

func WithActor(ctx context.Context, v string) context.Context {
	return context.WithValue(ctx, metaKey{"actor"}, v)
}

func WithCorrelation(ctx context.Context, v string) context.Context {
	return context.WithValue(ctx, metaKey{"correlation"}, v)
}

func WithCausation(ctx context.Context, v string) context.Context {
	return context.WithValue(ctx, metaKey{"causation"}, v)
}

func meta(ctx context.Context, name string) string {
	v, _ := ctx.Value(metaKey{name}).(string)
	return v
}

// TenantOf exposes the tenant for the consumer stand-in.
func TenantOf(ctx context.Context) string { t, _ := tenant(ctx); return t }

type streamKey struct {
	tenant string
	id     uuid.UUID
}

// InMemoryStore is the in-memory event log.
type InMemoryStore struct {
	codec  *Codec
	mu     sync.Mutex
	xid    uint64
	pos    int64
	events []Envelope
}

func NewInMemoryStore(codec *Codec) *InMemoryStore { return &InMemoryStore{codec: codec} }

// All returns every stored envelope (for the walkthrough output).
func (s *InMemoryStore) All() []Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Envelope(nil), s.events...)
}

// ReadAfter returns events after c (log order) whose type is in types (empty = all).
func (s *InMemoryStore) ReadAfter(c Cursor, types []string) []Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Envelope
	for _, e := range s.events {
		if e.Cursor.Position > c.Position && (len(types) == 0 || contains(types, e.EventType)) {
			out = append(out, e)
		}
	}
	return out
}

// ReadOne returns one event by tenant and id.
func (s *InMemoryStore) ReadOne(tenant string, id uuid.UUID) (Envelope, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.TenantID == tenant && e.EventID == id {
			return e, true
		}
	}
	return Envelope{}, false
}

// Head is the last position in the log.
func (s *InMemoryStore) Head() Cursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return Cursor{}
	}
	return s.events[len(s.events)-1].Cursor
}

// Decode exposes the codec for the consumer Router stand-in.
func (s *InMemoryStore) Decode(eventType string, data []byte) (aggregate.Event, error) {
	return s.codec.decode(eventType, data)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (s *InMemoryStore) load(k streamKey, to int) []Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Envelope
	for _, e := range s.events {
		if e.TenantID == k.tenant && e.StreamID == k.id && (to == 0 || e.StreamPosition <= to) {
			out = append(out, e)
		}
	}
	return out
}

type appendInput struct {
	key          streamKey
	streamType   string
	priorVersion int
	events       []aggregate.Event
	ctx          context.Context
}

func (s *InMemoryStore) append(in appendInput) (Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.TenantID == in.key.tenant && e.StreamID == in.key.id && e.StreamPosition > in.priorVersion {
			// Backstop: unique (tenant_id, stream_id, stream_position).
			return Cursor{}, ErrConcurrencyConflict
		}
	}
	s.xid++
	correlation := meta(in.ctx, "correlation")
	if correlation == "" {
		correlation = uuid.NewString()[28:] // a request starts a new chain
	}
	var last Cursor
	for i, ev := range in.events {
		data, err := json.Marshal(ev)
		if err != nil {
			return Cursor{}, fmt.Errorf("encode %s: %w", ev.EventType(), err)
		}
		s.pos++
		last = Cursor{TransactionID: s.xid, Position: s.pos}
		s.events = append(s.events, Envelope{
			Cursor: last, TenantID: in.key.tenant, StreamID: in.key.id, StreamType: in.streamType,
			StreamPosition: in.priorVersion + 1 + i, EventType: ev.EventType(), Data: data,
			OccurredOn: time.Now().UTC(),
			EventID:    uuid.Must(uuid.NewV7()), Actor: meta(in.ctx, "actor"),
			CorrelationID: correlation, CausationID: meta(in.ctx, "causation"),
		})
	}
	return last, nil
}

// --- command transaction (stand-in for DWX-209 eventstore.UnitOfWork) -------

type cmdTxKey struct{}

// InMemoryUnitOfWork serializes writers per (tenant, stream).
type InMemoryUnitOfWork struct {
	mu    sync.Mutex
	locks map[streamKey]*sync.Mutex
}

func NewInMemoryUnitOfWork() *InMemoryUnitOfWork {
	return &InMemoryUnitOfWork{locks: map[streamKey]*sync.Mutex{}}
}

// Execute opens the command transaction for stream and locks it before fn
// (and therefore before the load).
func (u *InMemoryUnitOfWork) Execute(ctx context.Context, stream uuid.UUID, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(cmdTxKey{}).(streamKey); ok {
		return ErrNestedUnitOfWork
	}
	t, err := tenant(ctx)
	if err != nil {
		return err
	}
	k := streamKey{tenant: t, id: stream}
	u.mu.Lock()
	l, ok := u.locks[k]
	if !ok {
		l = &sync.Mutex{}
		u.locks[k] = l
	}
	u.mu.Unlock()
	l.Lock()
	defer l.Unlock()
	return fn(context.WithValue(ctx, cmdTxKey{}, k))
}

func commandStream(ctx context.Context, stream uuid.UUID) (streamKey, error) {
	k, ok := ctx.Value(cmdTxKey{}).(streamKey)
	if !ok {
		return streamKey{}, ErrNotCommandTransaction
	}
	if k.id != stream {
		return streamKey{}, fmt.Errorf("tx holds %s, got %s: %w", k.id, stream, ErrStreamMismatch)
	}
	return k, nil
}
