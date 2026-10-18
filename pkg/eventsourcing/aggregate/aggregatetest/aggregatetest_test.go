package aggregatetest_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate/aggregatetest"
)

type TaskID uuid.UUID

type TaskCreated struct{ Title string }

func (TaskCreated) EventType() string { return "task.created" }

type TaskRenamed struct{ Title string }

func (TaskRenamed) EventType() string { return "task.renamed" }

type TaskCompleted struct{}

func (TaskCompleted) EventType() string { return "task.completed" }

type Task struct {
	aggregate.Base[TaskID]
	title string
	done  bool
}

func (*Task) StreamType() string { return "task" }

func (t *Task) Apply(e aggregate.Event) {
	switch e := e.(type) {
	case TaskCreated:
		t.title = e.Title
	case TaskRenamed:
		t.title = e.Title
	case TaskCompleted:
		t.done = true
	}
}

func (t *Task) Rename(title string) {
	if title != t.title {
		aggregate.Raise(t, TaskRenamed{Title: title})
	}
}

func (t *Task) Complete() {
	if !t.done {
		aggregate.Raise(t, TaskCompleted{})
	}
}

func TestNewThenRaise(t *testing.T) {
	t.Parallel()

	id := TaskID(uuid.MustParse("0192f1e4-7b6a-7c3e-9a51-3f2d8c4b1a00"))

	tests := []struct {
		name        string
		history     []aggregate.Event
		command     func(*Task)
		wantRaised  []aggregate.Event
		wantVersion int
		wantTitle   string
		wantDone    bool
	}{
		{
			name:        "empty history keeps zero state",
			command:     func(*Task) {},
			wantRaised:  nil,
			wantVersion: 0,
		},
		{
			name:        "history only",
			history:     []aggregate.Event{TaskCreated{Title: "a"}, TaskRenamed{Title: "b"}},
			command:     func(*Task) {},
			wantRaised:  nil,
			wantVersion: 2,
			wantTitle:   "b",
		},
		{
			name:    "command raises new events after history",
			history: []aggregate.Event{TaskCreated{Title: "a"}},
			command: func(task *Task) {
				task.Rename("b")
				task.Complete()
			},
			wantRaised:  []aggregate.Event{TaskRenamed{Title: "b"}, TaskCompleted{}},
			wantVersion: 3,
			wantTitle:   "b",
			wantDone:    true,
		},
		{
			name:        "no-op command raises nothing",
			history:     []aggregate.Event{TaskCreated{Title: "a"}, TaskCompleted{}},
			command:     func(task *Task) { task.Complete() },
			wantRaised:  nil,
			wantVersion: 2,
			wantTitle:   "a",
			wantDone:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			task := aggregatetest.New[TaskID, Task](id, tt.history...)
			if got := len(aggregatetest.Raised(task)); got != 0 {
				t.Fatalf("Raised after New = %d events, want 0", got)
			}
			if got := task.Version(); got != len(tt.history) {
				t.Fatalf("Version after New = %d, want %d", got, len(tt.history))
			}

			tt.command(task)

			if got := aggregatetest.Raised(task); !slices.Equal(got, tt.wantRaised) {
				t.Errorf("Raised = %v, want %v", got, tt.wantRaised)
			}
			if got := task.Version(); got != tt.wantVersion {
				t.Errorf("Version = %d, want %d", got, tt.wantVersion)
			}
			if task.ID() != id {
				t.Errorf("ID = %v, want %v", uuid.UUID(task.ID()), uuid.UUID(id))
			}
			if task.title != tt.wantTitle || task.done != tt.wantDone {
				t.Errorf("state = (%q, %v), want (%q, %v)", task.title, task.done, tt.wantTitle, tt.wantDone)
			}
		})
	}
}

func TestRaisedReturnsCopy(t *testing.T) {
	t.Parallel()

	task := aggregatetest.New[TaskID, Task](TaskID(uuid.New()), TaskCreated{Title: "a"})
	task.Rename("b")

	raised := aggregatetest.Raised(task)
	raised[0] = TaskCompleted{}

	want := []aggregate.Event{TaskRenamed{Title: "b"}}
	if got := aggregatetest.Raised(task); !slices.Equal(got, want) {
		t.Errorf("Raised = %v, want %v", got, want)
	}
}
