// Package app is the application layer of the quickstart service: its
// commands, its queries, the task_list Projection and the notify_assignee
// Reactor. The same App runs in memory and on Postgres; only the Store,
// UnitOfWork, Runtime and ReadModel passed to New differ.
package app

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/quickstart/todo"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/projection"
	"github.com/deepworx/go-utils/pkg/eventsourcing/reactor"
)

// Codec registers the service's events. Every service owns its Codec.
func Codec() *eventstore.Codec {
	c := eventstore.NewCodec()
	eventstore.Register[todo.TaskOpened](c)
	eventstore.Register[todo.TaskCompleted](c)
	eventstore.Register[todo.AssigneeNotified](c)
	return c
}

// Tasks is the Repository of Task Aggregates.
type Tasks = eventstore.Repository[todo.TaskID, todo.Task, *todo.Task]

// Config wires an App to one backend.
type Config struct {
	Codec      *eventstore.Codec
	Store      eventstore.Store
	UnitOfWork eventstore.UnitOfWork
	Runtime    consumer.Runtime
	ReadModel  ReadModel
	Mailer     Mailer
}

// App runs the service's commands and queries.
type App struct {
	tasks     *Tasks
	readModel ReadModel
	taskList  *projection.Projection
	notifier  *reactor.Reactor
}

// New wires the Repository, the task_list Projection and the
// notify_assignee Reactor.
func New(cfg Config) (*App, error) {
	a := &App{
		tasks: eventstore.NewRepository[todo.TaskID, todo.Task](eventstore.RepositoryConfig{
			Store:      cfg.Store,
			UnitOfWork: cfg.UnitOfWork,
		}),
		readModel: cfg.ReadModel,
	}
	var err error
	a.taskList, err = projection.New(cfg.Runtime, projection.Config{
		Name: "task_list",
		Active: projection.Version{
			N:       1,
			Handler: TaskListV1(cfg.Codec, cfg.ReadModel),
			Clear:   cfg.ReadModel.Clear,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("task_list projection: %w", err)
	}
	a.notifier, err = reactor.New(cfg.Runtime, reactor.Config{
		Name:    "notify_assignee",
		Handler: NotifyAssignee(cfg.Codec, a.tasks, cfg.Mailer),
		// The Reactor ships with the service's first release, so it reacts
		// to every event. A Reactor added to a service with history would
		// use FromNow.
		StartAt: reactor.FromBeginning,
	})
	if err != nil {
		return nil, fmt.Errorf("notify_assignee reactor: %w", err)
	}
	return a, nil
}

// Consumers returns every Consumer of the App, to run in one Group.
func (a *App) Consumers() consumer.Group {
	return append(consumer.Group(a.taskList.Consumers()), a.notifier.Consumer())
}

// OpenTask is the input of App.OpenTask.
type OpenTask struct {
	Title    string
	Assignee string
}

// OpenTask opens a new Task.
func (a *App) OpenTask(ctx context.Context, in OpenTask) (todo.TaskID, eventstore.CommandResult, error) {
	v7, err := uuid.NewV7()
	if err != nil {
		return todo.TaskID{}, eventstore.CommandResult{}, fmt.Errorf("task id: %w", err)
	}
	id := todo.TaskID(v7)
	res, err := a.tasks.Create(ctx, id, func(_ context.Context, t *todo.Task) error {
		return t.Open(in.Title, in.Assignee)
	})
	if err != nil {
		return todo.TaskID{}, eventstore.CommandResult{}, fmt.Errorf("open task: %w", err)
	}
	return id, res, nil
}

// CompleteTask marks a Task as done.
func (a *App) CompleteTask(ctx context.Context, id todo.TaskID) (eventstore.CommandResult, error) {
	res, err := a.tasks.Update(ctx, id, func(_ context.Context, t *todo.Task) error {
		return t.Complete()
	})
	if err != nil {
		return eventstore.CommandResult{}, fmt.Errorf("complete task: %w", err)
	}
	return res, nil
}

// ListTasks returns the Tenant's tasks once the task_list Projection has
// caught up to after, the Position of the caller's last command. When it
// has not caught up within the budget, the tasks are returned anyway,
// marked Stale.
func (a *App) ListTasks(ctx context.Context, after eventstore.Cursor) ([]Task, consumer.Freshness, error) {
	tenant, ok := ctxutil.TenantID(ctx)
	if !ok {
		return nil, 0, eventstore.ErrNoTenant
	}
	fresh := a.taskList.Await(ctx, after)
	tasks, err := a.readModel.List(ctx, tenant)
	if err != nil {
		return nil, 0, fmt.Errorf("list tasks: %w", err)
	}
	return tasks, fresh, nil
}
