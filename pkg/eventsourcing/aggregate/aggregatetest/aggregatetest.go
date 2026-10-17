// Package aggregatetest builds Aggregates from a history of events for
// domain unit tests, without a Repository, Codec or database.
//
// A test builds the Aggregate, runs a command and inspects what it raised:
//
//	task := aggregatetest.New[TaskID, Task](id, TaskCreated{Title: "a"})
//	task.Rename("b")
//	got := aggregatetest.Raised(task) // []aggregate.Event{TaskRenamed{Title: "b"}}
//
// Assertions are left to the test. Like aggregate, this package imports only
// the stdlib and uuid (plus the module-internal accessor).
package aggregatetest

import (
	"github.com/deepworx/go-utils/internal/aggregateaccess"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
)

// New allocates a zero-value A with the given id and replays history on it.
// The result has Version() == len(history) and nothing raised.
func New[I aggregate.ID, A any, PA interface {
	*A
	aggregate.Root[I]
}](id I, history ...aggregate.Event) *A {
	a := new(A)
	r := PA(a)
	aggregateaccess.Init[I](r, id)
	for _, e := range history {
		aggregateaccess.Replay[I](r, e)
	}
	return a
}

// Raised returns a copy of the events r raised since New, in raise order,
// or nil if none.
func Raised[I aggregate.ID](r aggregate.Root[I]) []aggregate.Event {
	return aggregateaccess.Uncommitted(r)
}
