// Package todo is the domain of the quickstart service. It imports only the
// aggregate package, the stdlib and uuid, which depguard enforces.
package todo

import (
	"errors"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
)

var (
	// ErrNoTitle reports opening a Task without a title.
	ErrNoTitle = errors.New("todo: task needs a title")
	// ErrAlreadyOpened reports opening a Task twice.
	ErrAlreadyOpened = errors.New("todo: task already opened")
	// ErrAlreadyDone reports completing a Task twice.
	ErrAlreadyDone = errors.New("todo: task already done")
)

// TaskID identifies a Task.
type TaskID uuid.UUID

// TaskOpened records a new Task.
type TaskOpened struct {
	Title    string `json:"title"`
	Assignee string `json:"assignee"`
}

// EventType returns the permanent Event type of TaskOpened.
func (TaskOpened) EventType() string { return "todo.task.opened" }

// TaskCompleted records that a Task is done.
type TaskCompleted struct{}

// EventType returns the permanent Event type of TaskCompleted.
func (TaskCompleted) EventType() string { return "todo.task.completed" }

// AssigneeNotified records that the assignee was told about the Task.
type AssigneeNotified struct{}

// EventType returns the permanent Event type of AssigneeNotified.
func (AssigneeNotified) EventType() string { return "todo.task.assignee_notified" }

// Task is the Aggregate of one task. Its zero value is a task not opened
// yet.
type Task struct {
	aggregate.Base[TaskID]
	opened   bool
	done     bool
	notified bool
}

// StreamType names the Task Aggregate.
func (*Task) StreamType() string { return "todo.task" }

// Apply changes the Task's state for one event.
func (t *Task) Apply(e aggregate.Event) {
	switch e.(type) {
	case TaskOpened:
		t.opened = true
	case TaskCompleted:
		t.done = true
	case AssigneeNotified:
		t.notified = true
	}
}

// Open opens the Task.
func (t *Task) Open(title, assignee string) error {
	if t.opened {
		return ErrAlreadyOpened
	}
	if title == "" {
		return ErrNoTitle
	}
	aggregate.Raise(t, TaskOpened{Title: title, Assignee: assignee})
	return nil
}

// Complete marks the Task as done.
func (t *Task) Complete() error {
	if t.done {
		return ErrAlreadyDone
	}
	aggregate.Raise(t, TaskCompleted{})
	return nil
}

// MarkNotified records that the assignee was notified. It is idempotent,
// because a Reactor may deliver the same event twice.
func (t *Task) MarkNotified() {
	if !t.notified {
		aggregate.Raise(t, AssigneeNotified{})
	}
}
