package consumer

import (
	"context"
	"time"

	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// Runtime runs Consumers on one event log and keeps their Cursors. It is
// implemented by PgRuntime and InMemoryRuntime.
type Runtime interface {
	// Resume clears the Stopped state of the named Consumer, which then
	// continues from its Cursor. It fails with ErrNotFound for an unknown
	// name and is a no-op when the Consumer is not Stopped.
	Resume(ctx context.Context, name string) error
	attach(in attachInput) (port, error)
}

type attachInput struct {
	name string
	// eventTypes is the filter; empty means every type.
	eventTypes []string
}

// port is what a Consumer needs from its Runtime: its Cursor, the filtered
// log, and Cursor transactions.
type port interface {
	codec() *eventstore.Codec
	// acquire takes the Consumer's lease. It fails with ErrRunning while
	// another runner holds it. The returned ctx ends when the lease is lost.
	acquire(ctx context.Context) (context.Context, func(), error)
	load(ctx context.Context) (cursorState, error)
	// read returns up to limit visible matching events after the Cursor
	// after, in Cursor order.
	read(ctx context.Context, in readInput) ([]record, error)
	// head returns the Cursor of the last visible matching event.
	head(ctx context.Context) (eventstore.Cursor, error)
	status(ctx context.Context) (Status, error)
	// frontier returns the transaction horizon and the first matching event
	// after the Cursor after, for Await.
	frontier(ctx context.Context, after eventstore.Cursor) (frontier, error)
	// transact runs fn in a Cursor transaction.
	transact(ctx context.Context, fn func(ctx context.Context) error) error
	// advance moves the Cursor if it is still at m.from. Inside a Cursor
	// transaction it commits with it.
	advance(ctx context.Context, m move) error
	stop(ctx context.Context, s Stop) error
	// changed returns a channel that receives or closes when new events may
	// be visible.
	changed() <-chan struct{}
	// resumed receives when Resume cleared the Stopped state.
	resumed() <-chan struct{}
}

type cursorState struct {
	cursor  eventstore.Cursor
	stopped *Stop
}

type readInput struct {
	after eventstore.Cursor
	limit int
}

// frontier is what lies ahead of a Cursor.
type frontier struct {
	// xmin is the transaction horizon: every transaction with a smaller ID
	// has ended (pg_snapshot_xmin).
	xmin uint64
	// next is the first matching event after the Cursor; zero when there
	// is none.
	next eventstore.Cursor
}

// covers reports whether no matching event can still appear in
// (Cursor, pos]: every transaction up to pos has ended and none of them
// wrote a matching event there.
func (f frontier) covers(pos eventstore.Cursor) bool {
	return pos.TransactionID < f.xmin && (f.next.IsZero() || f.next.Compare(pos) > 0)
}

type move struct {
	from, to eventstore.Cursor
}

// record is a read event before decoding.
type record struct {
	env     eventstore.Envelope
	payload eventstore.Payload
}

// PendingCap caps Status.Pending: a Consumer with more pending events
// reports PendingCap+1.
const PendingCap = 10_000

// Status is a Consumer's progress.
type Status struct {
	// Cursor is the last processed event.
	Cursor eventstore.Cursor
	// Pending counts the visible matching events after Cursor, up to
	// PendingCap+1.
	Pending int
	// Age is how long the oldest pending event has been in the log; zero
	// when nothing is pending.
	Age time.Duration
	// Stopped is set while the Consumer is Stopped.
	Stopped *Stop
}

// Stop describes why and when a Consumer went Stopped.
type Stop struct {
	At    time.Time
	Cause string
}
