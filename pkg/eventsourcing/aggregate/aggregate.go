package aggregate

import "github.com/deepworx/go-utils/internal/aggregateaccess/hook"

func init() {
	hook.Register(func(r any) *hook.State {
		return r.(stateHolder).state()
	})
}

// Event is a domain event. EventType returns its permanent Event type name.
type Event interface {
	EventType() string
}

// ID constrains typed Aggregate identifiers such as `type TaskID uuid.UUID`,
// so a TaskID cannot be passed where an OrderID is expected.
type ID interface {
	~[16]byte
}

// Root is an Aggregate as the event store sees it. It is sealed: only
// pointers to types that embed Base satisfy it.
type Root[I ID] interface {
	// StreamType names the kind of Aggregate. It must never change once
	// events have been written.
	StreamType() string
	// Version returns the number of events applied, historical and raised.
	Version() int
	// Apply changes state for one event. It is the single place where state
	// changes, for both historical and newly raised events, and must not
	// fail or have side effects.
	Apply(e Event)
	base() *Base[I]
}

// Base holds the event-sourcing bookkeeping of an Aggregate. Embed it by
// value. There is no constructor: the zero value of an Aggregate is its valid
// starting state, and the event store sets the ID when it allocates one.
type Base[I ID] struct {
	s hook.State
}

type stateHolder interface {
	state() *hook.State
}

// ID returns the Aggregate's typed identifier.
func (b *Base[I]) ID() I { return I(b.s.ID) }

// Version returns the number of events applied, historical and raised.
func (b *Base[I]) Version() int { return b.s.Version }

// base seals Root and ties it to the ID type: an Aggregate embedding
// Base[OrderID] does not satisfy Root[TaskID].
//
//nolint:unused // false positive: called through Root[I] in Raise
func (b *Base[I]) base() *Base[I] { return b }

func (b *Base[I]) state() *hook.State { return &b.s }

// Raise applies e to r, increments its version and records e as
// uncommitted, so the event store appends it on save.
func Raise[I ID](r Root[I], e Event) {
	s := &r.base().s
	r.Apply(e)
	s.Version++
	s.Uncommitted = append(s.Uncommitted, e)
}
