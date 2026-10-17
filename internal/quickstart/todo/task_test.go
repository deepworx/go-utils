package todo_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/quickstart/todo"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate/aggregatetest"
)

func TestTaskCommands(t *testing.T) {
	t.Parallel()
	id := todo.TaskID(uuid.New())
	opened := todo.TaskOpened{Title: "Write quickstart", Assignee: "alice@example.com"}

	tests := []struct {
		name    string
		history []aggregate.Event
		command func(*todo.Task) error
		wantErr error
		raised  []aggregate.Event
	}{
		{
			name:    "open raises TaskOpened",
			command: func(task *todo.Task) error { return task.Open(opened.Title, opened.Assignee) },
			raised:  []aggregate.Event{opened},
		},
		{
			name:    "open without title fails",
			command: func(task *todo.Task) error { return task.Open("", "") },
			wantErr: todo.ErrNoTitle,
		},
		{
			name:    "open twice fails",
			history: []aggregate.Event{opened},
			command: func(task *todo.Task) error { return task.Open(opened.Title, opened.Assignee) },
			wantErr: todo.ErrAlreadyOpened,
		},
		{
			name:    "complete raises TaskCompleted",
			history: []aggregate.Event{opened},
			command: func(task *todo.Task) error { return task.Complete() },
			raised:  []aggregate.Event{todo.TaskCompleted{}},
		},
		{
			name:    "complete twice fails",
			history: []aggregate.Event{opened, todo.TaskCompleted{}},
			command: func(task *todo.Task) error { return task.Complete() },
			wantErr: todo.ErrAlreadyDone,
		},
		{
			name:    "mark notified raises AssigneeNotified",
			history: []aggregate.Event{opened},
			command: func(task *todo.Task) error { task.MarkNotified(); return nil },
			raised:  []aggregate.Event{todo.AssigneeNotified{}},
		},
		{
			name:    "mark notified again raises nothing",
			history: []aggregate.Event{opened, todo.AssigneeNotified{}},
			command: func(task *todo.Task) error { task.MarkNotified(); return nil },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			task := aggregatetest.New[todo.TaskID, todo.Task](id, tt.history...)

			err := tt.command(task)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got := aggregatetest.Raised(task); !reflect.DeepEqual(got, tt.raised) {
				t.Fatalf("raised = %#v, want %#v", got, tt.raised)
			}
		})
	}
}
