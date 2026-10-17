package eventstore

import (
	"cmp"
	"time"

	"github.com/google/uuid"
)

// Cursor is a position in the event log. Cursors are ordered by
// (TransactionID, Position); the zero Cursor lies before every event.
type Cursor struct {
	TransactionID uint64
	Position      int64
}

// Compare returns -1, 0 or +1 when c lies before, at or after o.
func (c Cursor) Compare(o Cursor) int {
	if r := cmp.Compare(c.TransactionID, o.TransactionID); r != 0 {
		return r
	}
	return cmp.Compare(c.Position, o.Position)
}

// IsZero reports whether c is the zero Cursor.
func (c Cursor) IsZero() bool { return c == Cursor{} }

// Envelope is a stored event together with its log metadata. Event holds
// the decoded event, upcast to its current Schema version.
type Envelope struct {
	Cursor         Cursor
	EventID        uuid.UUID
	TenantID       string
	StreamType     string
	StreamID       uuid.UUID
	StreamPosition int
	EventType      string
	Event          any
	Actor          string
	CorrelationID  string
	// CausationID is uuid.Nil when the event has no Causation.
	CausationID uuid.UUID
	RecordedAt  time.Time
}

// CommandResult is what a command returns. Position is the Cursor of the
// last event written, for read-your-writes; it is zero when the command
// raised no events. Version is the Aggregate version after the command, the
// Expected version of the caller's next command.
type CommandResult struct {
	Position Cursor
	Version  int
}
