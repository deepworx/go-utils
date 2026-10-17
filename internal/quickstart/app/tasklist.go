package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/quickstart/todo"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/projection"
)

// Task is one row of the task_list read model.
type Task struct {
	ID       uuid.UUID `json:"id"`
	Title    string    `json:"title"`
	Assignee string    `json:"assignee"`
	Done     bool      `json:"done"`
	Notified bool      `json:"notified"`
	OpenedAt time.Time `json:"opened_at"`
}

// ReadModel stores the task_list read model. Its writes run in the Cursor
// transaction in ctx, so they commit together with the Projection's Cursor.
type ReadModel interface {
	Insert(ctx context.Context, tenant string, task Task) error
	SetDone(ctx context.Context, tenant string, id uuid.UUID) error
	SetNotified(ctx context.Context, tenant string, id uuid.UUID) error
	List(ctx context.Context, tenant string) ([]Task, error)
	// Clear empties the read model; only projection.Reset needs it.
	Clear(ctx context.Context) error
}

// TaskListV1 builds version 1 of the task_list Projection. Every write
// notifies subscribers, so a live UI can refetch the task.
func TaskListV1(codec *eventstore.Codec, rm ReadModel) *consumer.Router {
	r := consumer.NewRouter(codec)
	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, e todo.TaskOpened) error {
		err := rm.Insert(ctx, env.TenantID, Task{
			ID:       env.StreamID,
			Title:    e.Title,
			Assignee: e.Assignee,
			OpenedAt: env.RecordedAt,
		})
		if err != nil {
			return err
		}
		return projection.Notify(ctx, "task", env.StreamID)
	})
	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, _ todo.TaskCompleted) error {
		if err := rm.SetDone(ctx, env.TenantID, env.StreamID); err != nil {
			return err
		}
		return projection.Notify(ctx, "task", env.StreamID)
	})
	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, _ todo.AssigneeNotified) error {
		if err := rm.SetNotified(ctx, env.TenantID, env.StreamID); err != nil {
			return err
		}
		return projection.Notify(ctx, "task", env.StreamID)
	})
	return r
}
