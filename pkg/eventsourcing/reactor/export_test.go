package reactor

import (
	"context"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// Redeliver hands env to r's live handler once more, as the Consumer does
// after a crash between handling env and advancing the Cursor.
func Redeliver(ctx context.Context, r *Reactor, env eventstore.Envelope) error {
	ctx = ctxutil.WithTenantID(ctx, env.TenantID)
	ctx = ctxutil.WithCorrelationID(ctx, env.CorrelationID)
	return r.handle(ctx, env)
}

// DiscardedEvent is an audit record of the in-memory park list.
type DiscardedEvent struct {
	EventID uuid.UUID
	By      string
}

// Discarded returns the audit records the in-memory park list of r kept,
// in discard order.
func Discarded(r *Reactor) []DiscardedEvent {
	m, ok := r.parks.(*memParks)
	if !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DiscardedEvent, 0, len(m.discarded))
	for _, d := range m.discarded {
		out = append(out, DiscardedEvent{EventID: d.EventID, By: d.by})
	}
	return out
}
