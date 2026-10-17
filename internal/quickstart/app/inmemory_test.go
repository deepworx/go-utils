package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/deepworx/go-utils/internal/quickstart/app"
	"github.com/deepworx/go-utils/internal/quickstart/todo"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/projection"
)

func TestCodec(t *testing.T) {
	t.Parallel()
	eventstore.CheckCodec(t, app.Codec())
}

// TestInMemoryBoot boots the service the way the quickstart's in-memory
// section does and drives it like an API would.
func TestInMemoryBoot(t *testing.T) {
	t.Parallel()
	mailer := &recordingMailer{}

	codec := app.Codec()
	store := eventstore.NewInMemoryStore(codec)
	rt := consumer.NewInMemoryRuntime(consumer.InMemoryRuntimeConfig{Store: store})
	a, err := app.New(app.Config{
		Codec:      codec,
		Store:      store,
		UnitOfWork: eventstore.NewInMemoryUnitOfWork(),
		Runtime:    rt,
		ReadModel:  app.NewInMemoryReadModel(),
		Mailer:     mailer,
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	changes, err := projection.Subscribe(rt)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(changes.Close)

	runCtx, stop := context.WithCancel(t.Context())
	g, runCtx := errgroup.WithContext(runCtx)
	g.Go(func() error { return a.Consumers().Run(runCtx) })
	t.Cleanup(func() {
		stop()
		if err := g.Wait(); err != nil {
			t.Errorf("consumers: %v", err)
		}
	})

	ctx := ctxutil.WithTenantID(t.Context(), "default")
	ctx = ctxutil.WithActor(ctx, "user:alice")

	id, res, err := a.OpenTask(ctx, app.OpenTask{Title: "Write quickstart", Assignee: "alice@example.com"})
	if err != nil {
		t.Fatalf("open task: %v", err)
	}

	tasks, fresh, err := a.ListTasks(ctx, res.Position)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if fresh != consumer.Fresh {
		t.Fatalf("freshness = %v, want Fresh", fresh)
	}
	if len(tasks) != 1 || tasks[0].ID != uuid.UUID(id) || tasks[0].Title != "Write quickstart" {
		t.Fatalf("tasks = %+v, want the opened task", tasks)
	}

	select {
	case c := <-changes.C():
		want := projection.Change{Tenant: "default", Entity: "task", ID: uuid.UUID(id)}
		if c != want {
			t.Fatalf("change = %+v, want %+v", c, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no change notification")
	}

	waitFor(t, "reactor follow-up command", func() bool {
		tasks, _, err := a.ListTasks(ctx, eventstore.Cursor{})
		return err == nil && len(tasks) == 1 && tasks[0].Notified
	})
	if got := mailer.sent(); len(got) != 1 || got[0].To != "alice@example.com" || got[0].IdempotencyKey == "" {
		t.Fatalf("mails = %+v, want one to alice with an idempotency key", got)
	}

	res, err = a.CompleteTask(ctx, id)
	if err != nil {
		t.Fatalf("complete task: %v", err)
	}
	tasks, fresh, err = a.ListTasks(ctx, res.Position)
	if err != nil || fresh != consumer.Fresh || !tasks[0].Done {
		t.Fatalf("after complete: tasks = %+v, freshness = %v, err = %v", tasks, fresh, err)
	}
	if _, err := a.CompleteTask(ctx, id); !errors.Is(err, todo.ErrAlreadyDone) {
		t.Fatalf("second complete: err = %v, want todo.ErrAlreadyDone", err)
	}
}

type recordingMailer struct {
	mu    sync.Mutex
	mails []app.Mail
}

func (m *recordingMailer) Send(_ context.Context, mail app.Mail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mails = append(m.mails, mail)
	return nil
}

func (m *recordingMailer) sent() []app.Mail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]app.Mail(nil), m.mails...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
