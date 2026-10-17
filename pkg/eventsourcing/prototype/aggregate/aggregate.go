// PROTOTYPE (DWX-212): throwaway API sketch, not production code.
//
// Package aggregate is the only package a domain imports: stdlib + uuid.
package aggregate

import "github.com/google/uuid"

// Event is a domain event. The schema version is derived by the codec
// (DWX-210), so only the permanent type name is asked for.
type Event interface {
	EventType() string
}

// ID constrains strongly-typed identifiers: `type TaskID uuid.UUID`.
// Replaces CPQ's AggregateID interface (Type()+ID()) with a type set, so a
// TaskID can't be passed where an OrderID is expected and needs no methods.
type ID interface {
	~[16]byte
}

// Root is what the repository needs from an aggregate. The unexported base
// method is only satisfied by embedding Base, so every Root has the
// version/uncommitted bookkeeping and nobody re-implements it.
type Root[I ID] interface {
	StreamType() string
	Version() int
	// Apply mutates state for one event. Used for both new and historical
	// events, so there is exactly one place where state changes.
	Apply(Event)
	base() *Base[I]
}

// Base holds the event-sourcing bookkeeping. Embed it by value.
type Base[I ID] struct {
	id          I
	version     int
	uncommitted []Event
}

// NewBase returns a Base for a fresh, zero-version aggregate.
func NewBase[I ID](id I) Base[I] { return Base[I]{id: id} }

// ID returns the typed identifier.
func (b *Base[I]) ID() I { return b.id }

// Version returns the number of events applied (historical + uncommitted).
func (b *Base[I]) Version() int { return b.version }

func (b *Base[I]) base() *Base[I] { return b }

// Raise records a new event: Apply, version++, track as uncommitted.
//
// Package function instead of CPQ's b.RaiseEvent(e, a.apply): Base can't call
// the embedding type's Apply, so CPQ passes the applier on every call. Here
// the Root brings its own Apply.
func Raise[I ID](r Root[I], e Event) {
	b := r.base()
	r.Apply(e)
	b.version++
	b.uncommitted = append(b.uncommitted, e)
}

// --- infrastructure access, used by eventstore only -------------------------
// These are exported because eventstore lives in another package. They are
// the "back door"; documented as not for domain use.

// UUID returns the stream id of r.
func UUID[I ID](r Root[I]) uuid.UUID { return uuid.UUID(r.base().id) }

// Uncommitted returns events raised since the last save.
func Uncommitted[I ID](r Root[I]) []Event { return r.base().uncommitted }

// MarkCommitted clears the uncommitted events after a successful save.
func MarkCommitted[I ID](r Root[I]) { r.base().uncommitted = nil }

// Replay applies a historical event without tracking it as uncommitted.
func Replay[I ID](r Root[I], e Event) {
	r.Apply(e)
	r.base().version++
}
