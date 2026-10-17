// PROTOTYPE (DWX-212): example domain. Imports only aggregate.
package todo

import (
	"errors"

	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/aggregate"
)

var (
	ErrAlreadyOpened = errors.New("todo: task already opened")
	ErrEmptyTitle    = errors.New("todo: title required")
	ErrDone          = errors.New("todo: task already done")
	ErrArchived      = errors.New("todo: task already archived")
	ErrNotDone       = errors.New("todo: task not done")
)

type TaskID [16]byte // real code: type TaskID uuid.UUID

type TaskOpened struct {
	Title string `json:"title"`
}

func (TaskOpened) EventType() string { return "Todo.Task.Opened" }

type TaskRenamed struct {
	Title string `json:"title"`
}

func (TaskRenamed) EventType() string { return "Todo.Task.Renamed" }

type TaskCompleted struct{}

func (TaskCompleted) EventType() string { return "Todo.Task.Completed" }

// TaskArchived is raised by the crm_sync Reactor (DWX-221 prototype).
type TaskArchived struct {
	CRMRef string `json:"crm_ref"`
}

func (TaskArchived) EventType() string { return "Todo.Task.Archived" }

// Task is the example aggregate.
type Task struct {
	aggregate.Base[TaskID]
	title    string
	done     bool
	archived bool
}

func (t *Task) StreamType() string { return "Todo.Task" }
func (t *Task) Title() string      { return t.title }
func (t *Task) Done() bool         { return t.done }

func (t *Task) Apply(e aggregate.Event) {
	switch e := e.(type) {
	case TaskOpened:
		t.title = e.Title
	case TaskRenamed:
		t.title = e.Title
	case TaskCompleted:
		t.done = true
	case TaskArchived:
		t.archived = true
	}
}

// Open is the creation command, run on New(id) by Repository.Create.
func (t *Task) Open(title string) error {
	if t.Version() != 0 {
		return ErrAlreadyOpened
	}
	if title == "" {
		return ErrEmptyTitle
	}
	aggregate.Raise(t, TaskOpened{Title: title})
	return nil
}

func (t *Task) Rename(title string) error {
	if title == "" {
		return ErrEmptyTitle
	}
	if title == t.title {
		return nil // idempotent: no event
	}
	aggregate.Raise(t, TaskRenamed{Title: title})
	return nil
}

func (t *Task) Complete() error {
	if t.done {
		return ErrDone
	}
	aggregate.Raise(t, TaskCompleted{})
	return nil
}

// Archive is the command the Reactor sends once the CRM has closed the deal.
func (t *Task) Archive(crmRef string) error {
	if !t.done {
		return ErrNotDone
	}
	if t.archived {
		return ErrArchived
	}
	aggregate.Raise(t, TaskArchived{CRMRef: crmRef})
	return nil
}

func (t *Task) Archived() bool { return t.archived }
