package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/consumerhook"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

// ErrOutsideHandler reports Notify called outside the Handler of a
// Projection version.
var ErrOutsideHandler = errors.New("projection: notify outside a projection handler")

// Change tells that a read model entity changed, so a live UI can refetch
// it.
type Change struct {
	Tenant string
	Entity string
	ID     uuid.UUID
	// Gap reports that changes may have been lost (a full buffer or a
	// reconnect); the other fields are empty. Refetch everything shown.
	Gap bool
}

// changePayload is the JSON payload on the <schema>.changes channel.
type changePayload struct {
	Tenant string    `json:"tenant"`
	Entity string    `json:"entity"`
	ID     uuid.UUID `json:"id"`
}

type notifierKey struct{}

// notifier is what Notify finds in a version Handler's ctx. Its changes is
// nil in the shadow.
type notifier struct {
	changes consumerhook.Changes
}

// handler runs a version's Handler with its notifier in ctx.
type handler struct {
	inner    consumer.Handler
	notifier *notifier
}

func newHandler(inner consumer.Handler, changes consumerhook.Changes) *handler {
	return &handler{inner: inner, notifier: &notifier{changes: changes}}
}

func (h *handler) Handle(ctx context.Context, env eventstore.Envelope) error {
	return h.inner.Handle(context.WithValue(ctx, notifierKey{}, h.notifier), env)
}

// EventTypes passes on the filter of the wrapped Handler, if it has one.
func (h *handler) EventTypes() []string {
	if typer, ok := h.inner.(interface{ EventTypes() []string }); ok {
		return typer.EventTypes()
	}
	return nil
}

// Notify sends a Change for the entity id of the Tenant in ctx to every
// Subscription. Call it from the Handler of a Projection version: it is
// sent inside the Cursor transaction, so subscribers only see it once the
// transaction commits, and never when the handler fails. In the shadow
// version it does nothing. Outside a handler it fails with
// ErrOutsideHandler.
//
// A PgRuntime sends it with pg_notify on the channel <schema>.changes; an
// InMemoryRuntime delivers it in process.
func Notify(ctx context.Context, entity string, id uuid.UUID) error {
	n, ok := ctx.Value(notifierKey{}).(*notifier)
	switch {
	case !ok:
		return ErrOutsideHandler
	case n.changes == nil:
		return nil
	case entity == "":
		return errors.New("projection: notify: empty entity")
	}
	tenant, _ := ctxutil.TenantID(ctx)
	payload, err := json.Marshal(changePayload{Tenant: tenant, Entity: entity, ID: id})
	if err != nil {
		return fmt.Errorf("projection: notify: %w", err)
	}
	if err := n.changes.Publish(ctx, string(payload)); err != nil {
		return fmt.Errorf("projection: notify %s %s: %w", entity, id, err)
	}
	return nil
}

// Subscription receives the Changes of one Runtime.
type Subscription struct {
	feed   consumerhook.Feed
	out    chan Change
	done   chan struct{}
	exited chan struct{}
	once   sync.Once
}

// Subscribe returns a Subscription to the Changes sent on rt from now on.
// Close it when done. On a PgRuntime it listens through the runtime's
// Listener and fails with consumer.ErrInvalidConfig when there is none;
// changes sent before the LISTEN took effect are not delivered.
func Subscribe(rt consumer.Runtime) (*Subscription, error) {
	changes, ok := consumerhook.RuntimeChanges(rt)
	if !ok {
		return nil, fmt.Errorf("projection: subscribe: unknown runtime %T: %w", rt, consumer.ErrInvalidConfig)
	}
	feed, err := changes.Subscribe()
	if err != nil {
		return nil, fmt.Errorf("projection: %w", err)
	}
	s := &Subscription{
		feed:   feed,
		out:    make(chan Change),
		done:   make(chan struct{}),
		exited: make(chan struct{}),
	}
	go s.forward()
	return s, nil
}

// forward decodes the feed into s.out until Close or the end of the feed.
func (s *Subscription) forward() {
	defer close(s.exited)
	defer close(s.out)
	for {
		select {
		case <-s.done:
			return
		case n, ok := <-s.feed.C():
			if !ok {
				return
			}
			select {
			case s.out <- decode(n):
			case <-s.done:
				return
			}
		}
	}
}

// decode turns a notification into a Change. A payload it cannot read is
// reported as a Gap.
func decode(n notify.Notification) Change {
	if n.Gap {
		return Change{Gap: true}
	}
	var p changePayload
	if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
		return Change{Gap: true}
	}
	return Change{Tenant: p.Tenant, Entity: p.Entity, ID: p.ID}
}

// C returns the channel Changes are delivered on. It is closed by Close.
func (s *Subscription) C() <-chan Change { return s.out }

// Close ends the Subscription and closes C. Close is idempotent.
func (s *Subscription) Close() {
	s.once.Do(func() {
		close(s.done)
		<-s.exited
		s.feed.Close()
	})
}
