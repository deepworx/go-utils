// PROTOTYPE (DWX-212): walkthrough of the aggregate/repository API.
//
//	go run ./pkg/eventsourcing/prototype
package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/todo"
)

// TaskService is what an app's command handler looks like on the library.
type TaskService struct {
	tasks *eventstore.Repository[todo.TaskID, todo.Task, *todo.Task]
	uow   *eventstore.InMemoryUnitOfWork
}

func (s *TaskService) Open(ctx context.Context, id todo.TaskID, title string) (eventstore.CommandResult, error) {
	return s.tasks.Create(ctx, id, func(ctx context.Context, t *todo.Task) error {
		return t.Open(title)
	})
}

func (s *TaskService) Rename(ctx context.Context, id todo.TaskID, title string, expected int) (eventstore.CommandResult, error) {
	return s.tasks.Update(ctx, id, func(ctx context.Context, t *todo.Task) error {
		return t.Rename(title)
	}, eventstore.ExpectVersion(expected))
}

func (s *TaskService) Complete(ctx context.Context, id todo.TaskID) (eventstore.CommandResult, error) {
	return s.tasks.Update(ctx, id, func(ctx context.Context, t *todo.Task) error {
		return t.Complete()
	})
}

// RenameLowLevel is the escape hatch: same command via Execute + Load + Save.
func (s *TaskService) RenameLowLevel(ctx context.Context, id todo.TaskID, title string) (eventstore.CommandResult, error) {
	var res eventstore.CommandResult
	err := s.uow.Execute(ctx, uuid.UUID(id), func(ctx context.Context) error {
		t, err := s.tasks.Load(ctx, id)
		if err != nil {
			return err
		}
		if err := t.Rename(title); err != nil {
			return err
		}
		res, err = s.tasks.Save(ctx, t)
		return err
	})
	return res, err
}

func main() {
	codec := eventstore.NewCodec()
	eventstore.Register[todo.TaskOpened](codec)
	eventstore.Register[todo.TaskRenamed](codec)
	eventstore.Register[todo.TaskCompleted](codec)

	store := eventstore.NewInMemoryStore(codec)
	uow := eventstore.NewInMemoryUnitOfWork()
	tasks := eventstore.NewRepository[todo.TaskID, todo.Task](eventstore.RepositoryConfig{
		Store: store, UnitOfWork: uow,
	})
	svc := &TaskService{tasks: tasks, uow: uow}

	acme := eventstore.WithTenant(context.Background(), "acme")
	globex := eventstore.WithTenant(context.Background(), "globex")
	id := todo.TaskID(uuid.Must(uuid.NewV7()))

	step := func(name string, res eventstore.CommandResult, err error) {
		fmt.Printf("\n▶ %s\n  result: %+v\n  err:    %v\n", name, res, err)
	}
	show := func(ctx context.Context) {
		t, err := tasks.Load(ctx, id)
		if err != nil {
			fmt.Printf("  state:  %v\n", err)
			return
		}
		fmt.Printf("  state:  version=%d title=%q done=%v\n", t.Version(), t.Title(), t.Done())
	}

	res, err := svc.Open(acme, id, "write spec")
	step("Open (acme)", res, err)
	show(acme)

	res, err = svc.Open(acme, id, "again")
	step("Open same id again (acme) → ErrAlreadyExists", res, err)

	res, err = svc.Open(globex, id, "same uuid, other tenant")
	step("Open same id in globex → independent stream (DWX-211)", res, err)
	show(globex)

	res, err = svc.Rename(acme, id, "write API spec", 1)
	step("Rename expecting v1", res, err)
	show(acme)

	res, err = svc.Rename(acme, id, "stale UI", 1)
	step("Rename expecting stale v1 → ErrConcurrencyConflict", res, err)

	res, err = svc.Rename(acme, id, "write API spec", 2)
	step("Rename to same title → idempotent, zero Position", res, err)

	res, err = svc.Complete(acme, id)
	step("Complete (no expected version)", res, err)
	show(acme)

	res, err = svc.Complete(acme, id)
	step("Complete again → domain error, nothing written", res, err)

	res, err = svc.RenameLowLevel(acme, id, "low-level path")
	step("RenameLowLevel (Execute + Load + Save)", res, err)
	show(acme)

	t, _ := tasks.LoadAt(acme, id, 2)
	fmt.Printf("\n▶ LoadAt v2\n  state:  version=%d title=%q done=%v\n", t.Version(), t.Title(), t.Done())

	_, err = tasks.LoadAt(acme, id, 9)
	fmt.Printf("\n▶ LoadAt v9 → ErrNotFound\n  err:    %v\n", err)

	t, _ = tasks.Load(acme, id)
	_ = t.Rename("outside tx")
	res, err = tasks.Save(acme, t)
	step("Save outside a command transaction → ErrNotCommandTransaction", res, err)

	_, err = svc.Complete(context.Background(), id)
	fmt.Printf("\n▶ Command without tenant → ErrNoTenant\n  err:    %v\n", err)

	fmt.Println("\n── event log ──")
	for _, e := range store.All() {
		fmt.Printf("  %-7s %s v%d %-22s %s  @%+v\n", e.TenantID, uuid.UUID(e.StreamID).String()[:8], e.StreamPosition, e.EventType, e.Data, e.Cursor)
	}
}
