// Package aggregateaccess is the infrastructure side of package aggregate.
// It reaches the bookkeeping that aggregate.Base keeps unexported. Only
// eventstore and aggregatetest may import it (enforced by depguard).
package aggregateaccess

import (
	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/aggregateaccess/hook"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
)

// Init sets the ID of a freshly allocated, zero-version Aggregate.
func Init[I aggregate.ID](r aggregate.Root[I], id I) {
	hook.StateOf(r).ID = uuid.UUID(id)
}

// UUID returns the stream ID of r.
func UUID[I aggregate.ID](r aggregate.Root[I]) uuid.UUID {
	return hook.StateOf(r).ID
}

// Uncommitted returns a copy of the events raised since the last
// MarkCommitted, in raise order.
func Uncommitted[I aggregate.ID](r aggregate.Root[I]) []aggregate.Event {
	pending := hook.StateOf(r).Uncommitted
	if len(pending) == 0 {
		return nil
	}
	events := make([]aggregate.Event, len(pending))
	for i, e := range pending {
		events[i] = e
	}
	return events
}

// MarkCommitted forgets the uncommitted events after a successful save.
func MarkCommitted[I aggregate.ID](r aggregate.Root[I]) {
	hook.StateOf(r).Uncommitted = nil
}

// Replay applies a historical event and increments the version without
// recording the event as uncommitted.
func Replay[I aggregate.ID](r aggregate.Root[I], e aggregate.Event) {
	r.Apply(e)
	hook.StateOf(r).Version++
}
