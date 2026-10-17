<!--
PROTOTYPE (DWX-226): throwaway sketch of the future quickstart, written against
the decided-but-unbuilt API of map DWX-205. It does not compile. Every
`GAP:` marker is a question the map has not answered yet.
-->

# Event sourcing quickstart

A minimal service: one Aggregate (`Task`), one Projection (`task_list`), one
Reactor (`notify_assignee`). It boots in memory for prototypes and on Postgres
for real.

```
myservice/
  internal/todo/            domain: imports only pkg/eventsourcing/aggregate
  internal/app/             commands, queries, wiring of Repository + Consumers
  internal/readmodel/       projection handlers (sqlc)
  migrations/               golang-migrate: 000001 library schema, 000002+ app
  cmd/server/main.go
```

## 1. Domain

```go
package todo

import (
	"errors"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
)

type TaskID uuid.UUID

var ErrDone = errors.New("todo: task already done")

type TaskOpened struct {
	Title    string `json:"title"`
	Assignee string `json:"assignee"`
}

func (TaskOpened) EventType() string { return "Todo.Task.Opened" }

type TaskCompleted struct{}

func (TaskCompleted) EventType() string { return "Todo.Task.Completed" }

// Task's zero value is a valid, not-yet-opened task: the Repository allocates it.
type Task struct {
	aggregate.Base[TaskID]
	done bool
}

func (t *Task) StreamType() string { return "Todo.Task" }

func (t *Task) Apply(e aggregate.Event) {
	switch e.(type) {
	case TaskCompleted:
		t.done = true
	}
}

func (t *Task) Open(title, assignee string) error {
	aggregate.Raise(t, TaskOpened{Title: title, Assignee: assignee})
	return nil
}

func (t *Task) Complete() error {
	if t.done {
		return ErrDone
	}
	aggregate.Raise(t, TaskCompleted{})
	return nil
}
```

Unit test without any store (`aggregatetest`, DWX-224):

```go
func TestCompleteTwice(t *testing.T) {
	task := aggregatetest.New[todo.TaskID, todo.Task](id, todo.TaskOpened{Title: "x"}, todo.TaskCompleted{})
	require.ErrorIs(t, task.Complete(), todo.ErrDone)
	require.Empty(t, aggregatetest.Raised(task))
}
```

## 2. Codec

```go
func Codec() *eventstore.Codec {
	c := eventstore.NewCodec()
	eventstore.Register[todo.TaskOpened](c)    // schema version 1
	eventstore.Register[todo.TaskCompleted](c)
	return c
}

func TestCodec(t *testing.T) { eventstore.CheckCodec(t, Codec()) }
```

## 3. Commands and read-your-write

```go
type Tasks = eventstore.Repository[todo.TaskID, todo.Task, *todo.Task]

type App struct {
	tasks    *Tasks
	taskList *projection.Projection
	queries  *readmodel.Queries
}

func (a *App) OpenTask(ctx context.Context, title, assignee string) (todo.TaskID, eventstore.CommandResult, error) {
	id := todo.TaskID(uuid.Must(uuid.NewV7()))
	res, err := a.tasks.Create(ctx, id, func(_ context.Context, t *todo.Task) error {
		return t.Open(title, assignee)
	})
	return id, res, err
}

// The client sends back res.Position from the last command it issued.
func (a *App) ListTasks(ctx context.Context, after eventstore.Cursor) ([]readmodel.Task, consumer.Freshness, error) {
	fresh, err := a.taskList.Await(ctx, after) // 2s default budget; Stale is not an error
	if err != nil {
		return nil, 0, err
	}
	tasks, err := a.queries.ListTasks(ctx, postgres.Conn(ctx, a.pool))
	return tasks, fresh, err
}
```

The tenant comes from `ctxutil.TenantID(ctx)` (explicit value, else claims).
GAP: which constructor takes `WithDefaultTenant("…")`: the Store, the
command UnitOfWork, or the Repository? It has to cover reads (`Load`) and
writes alike, so the Store is the likely home.

## 4. Projection

The service's migration owns the table; the library only runs the handler
inside the Cursor transaction.

```sql
-- migrations/000002_task_list.up.sql
CREATE TABLE task_list_v1 (
  tenant_id TEXT NOT NULL,
  id        UUID NOT NULL,
  title     TEXT NOT NULL,
  done      BOOLEAN NOT NULL DEFAULT false,
  PRIMARY KEY (tenant_id, id)
);
CREATE VIEW task_list AS SELECT * FROM task_list_v1;
```

```go
package readmodel

func TaskListV1(codec *eventstore.Codec, q *Queries) consumer.Handler {
	r := consumer.NewRouter(codec)
	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, e todo.TaskOpened) error {
		// postgres.Conn returns the Cursor transaction.
		err := q.InsertTaskV1(ctx, postgres.Conn(ctx, nil), InsertTaskV1Params{
			TenantID: env.TenantID, ID: env.StreamID, Title: e.Title,
		})
		if err != nil {
			return err
		}
		return projection.Notify(ctx, "task", env.StreamID) // GAP: signature, see below
	})
	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, _ todo.TaskCompleted) error {
		return q.CompleteTaskV1(ctx, postgres.Conn(ctx, nil), env.TenantID, env.StreamID)
	})
	return r
}

taskList, err := projection.New(rt, projection.Config{
	Name:   "task_list",
	Active: projection.Version{N: 1, Handler: readmodel.TaskListV1(codec, q), Clear: q.TruncateTaskListV1},
})
```

GAPs from writing this:
- `projection.Notify(ctx, payload)` (DWX-220) and the `changes` payload
  `{"tenant","entity","id"}` (DWX-211) disagree on who builds the JSON. The
  sketch assumes `Notify(ctx, entity string, id uuid.UUID)` with the tenant
  taken from ctx. Who listens? There's no `projection.Subscribe` / changes
  listener API on the map.
- `postgres.Conn(ctx, pool)` needs a pool argument, which a handler doesn't
  have. Inside the Cursor transaction the pool is never used. Pass nil, or add
  `postgres.MustTx(ctx)`?
- `Clear` exists only for `Reset` (prototypes). Should it be optional?

## 5. Reactor

```go
func NotifyAssignee(codec *eventstore.Codec, mail Mailer) consumer.Handler {
	r := consumer.NewRouter(codec)
	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, e todo.TaskOpened) error {
		// at least once: EventID is the idempotency key
		err := mail.Send(ctx, mail.Message{To: e.Assignee, Subject: e.Title, IdempotencyKey: env.EventID.String()})
		if errors.Is(err, ErrInvalidAddress) {
			return consumer.Permanent(err) // park the stream at once
		}
		return err
	})
	return r
}

notify, err := reactor.New(rt, reactor.Config{
	Name:    "notify_assignee",
	Handler: NotifyAssignee(codec, mail),
	StartAt: reactor.FromNow,
})
```

## 6a. Boot path: in memory (prototypes, app-service tests)

```go
codec := app.Codec()
store := eventstore.NewInMemoryStore(codec, eventstore.WithDefaultTenant("dev"))
uow := eventstore.NewInMemoryUnitOfWork()
tasks := eventstore.NewRepository[todo.TaskID, todo.Task](eventstore.RepositoryConfig{Store: store, UnitOfWork: uow})
rt := consumer.NewInMemoryRuntime(consumer.InMemoryRuntimeConfig{Store: store})

taskList, _ := projection.New(rt, projection.Config{Name: "task_list", Active: projection.Version{N: 1, Handler: readmodel.TaskListInMemory(codec, rm)}})
notify, _ := reactor.New(rt, reactor.Config{Name: "notify_assignee", Handler: NotifyAssignee(codec, mail), StartAt: reactor.FromNow})

go consumer.Group{taskList.Consumers()..., notify.Consumer()}.Run(ctx)
```

GAP (the biggest one this sketch found): in memory, the projection handler can't
be the sqlc handler, because there's no Postgres. So either
- (a) the prototype writes a second, map-backed handler that registers its
  writes with `postgres.Stage(ctx, func(){ … })`, so they roll back with the
  Cursor transaction (the sketch assumes this; `readmodel.TaskListInMemory`), or
- (b) the in-memory boot path covers only the write side and Reactors, and
  projections always need Postgres (testcontainers in tests, `docker compose`
  for prototypes).
DWX-215 ships "an in-memory Consumer runtime" but never says what an in-memory
read model is.

Tests drive everything synchronously: `taskList.Consumers()[0].Drain(ctx)`,
`notify.Drain(ctx)`.

## 6b. Boot path: Postgres

```sh
cp $(go env GOMODCACHE)/github.com/deepworx/go-utils@vX.Y.Z/docs/postgres/schema.sql \
   migrations/000001_eventsourcing.up.sql
# delete the sections for packages you don't use (scheduler, inbox, …)
```

```go
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	shutdown, _ := otel.Setup(ctx, otelCfg) // global providers; native histograms
	defer shutdown(context.Background())

	pool, _ := postgres.NewPool(ctx, pgCfg)
	codec := app.Codec()

	store := eventstore.NewPgStore(eventstore.PgStoreConfig{Pool: pool, Codec: codec}) // Schema defaults to "public"
	uow := eventstore.NewPgUnitOfWork(postgres.NewUnitOfWork(pool))
	tasks := eventstore.NewRepository[todo.TaskID, todo.Task](eventstore.RepositoryConfig{Store: store, UnitOfWork: uow})

	listener := notify.NewListener(pool, notify.Config{ /* GAP: Schema? channels? */ })
	claims := lease.NewClaims(pool /* GAP: Schema? */)
	rt := consumer.NewPgRuntime(consumer.PgRuntimeConfig{Pool: pool, Store: store, Listener: listener, Claims: claims})

	taskList, _ := projection.New(rt, projection.Config{Name: "task_list", Active: projection.Version{N: 1, Handler: readmodel.TaskListV1(codec, q)}})
	notifyAssignee, _ := reactor.New(rt, reactor.Config{Name: "notify_assignee", Handler: NotifyAssignee(codec, mail), StartAt: reactor.FromNow})

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return listener.Run(ctx) })
	g.Go(func() error { return claims.Run(ctx) }) // GAP: does Claims need a loop?
	g.Go(func() error {
		return consumer.Group{append(taskList.Consumers(), notifyAssignee.Consumer())...}.Run(ctx)
	})
	g.Go(func() error { return serveHTTP(ctx, app.New(tasks, taskList, q)) })
	_ = g.Wait()
}
```

GAPs:
- The `Schema` is said to be "on every `Pg*` constructor" (DWX-213), but the
  runtime "takes it from the Store" (DWX-220). `notify.Listener` and
  `lease.Claims` live in `pkg/postgres/*` and don't know about the Store.
  Do they take `Schema` too, or channel names / key prefixes derived by the
  caller?
- `pkg/postgres/notify` and `pkg/postgres/lease` have no decided API at all
  (constructor, `Run`, how a Consumer subscribes, how a lease is lost).

## 7. Guide: bumping a projection version

Change the shape or repair the data → new version. Three releases, all with
golang-migrate.

**Release N: build the shadow**

```sql
-- 000007_task_list_v2.up.sql
CREATE TABLE task_list_v2 (
  tenant_id TEXT NOT NULL,
  id        UUID NOT NULL,
  title     TEXT NOT NULL,
  done      BOOLEAN NOT NULL DEFAULT false,
  assignee  TEXT,                          -- new column, appended
  PRIMARY KEY (tenant_id, id)
);
```
```go
projection.Config{
	Name:   "task_list",
	Active: projection.Version{N: 1, Handler: readmodel.TaskListV1(codec, q)},
	Shadow: &projection.Version{N: 2, Handler: readmodel.TaskListV2(codec, q)},
}
```
Both `task_list@v1` and `task_list@v2` run. Readers, `Await` and `Notify` stay
on v1. Watch `eventsourcing.consumer.pending{consumer="task_list@v2"}` and
`p.CaughtUp(ctx)`.

**Release N+1: switch**

```sql
-- 000008_task_list_switch.up.sql
DO $$
BEGIN
  IF consumer_pending('task_list@v2', 1001) > 1000 THEN
    RAISE EXCEPTION 'task_list@v2 has not caught up; retry the rollout later';
  END IF;
END $$;

CREATE OR REPLACE VIEW task_list AS SELECT * FROM task_list_v2;
```
```go
projection.Config{Name: "task_list", Active: projection.Version{N: 2, Handler: readmodel.TaskListV2(codec, q)}}
```

**Column-compatibility rule:** vN+1 keeps every column of vN with the same
name, type and position, and only appends new ones. Postgres enforces exactly
this: `CREATE OR REPLACE VIEW` refuses anything else. So the rule is "always
switch with `CREATE OR REPLACE VIEW`, never `DROP VIEW`". To drop or retype a
column, do one bump that stops reading it, then a later bump that removes it.

**Release N+2: clean up**

```sql
DROP TABLE task_list_v1;
DELETE FROM consumer_cursors WHERE name = 'task_list@v1';
```

GAPs from the guide:
- `consumer_pending` for a name with no Cursor row (shadow never deployed, or
  a typo) must raise, not return 0. Otherwise the guard passes on an empty v2.
- The guard doesn't see Stopped. A shadow Stopped on the last event has
  pending = 1 and passes a threshold of 1000. Should `consumer_pending` return
  NULL / raise for a Stopped Cursor, or should the snippet also check
  `consumer_cursors.stopped`?
- During the N+1 rollout, old pods still `Await` v1 while the view already
  shows v2, so read-your-write can be briefly stale. This is acceptable
  (Freshness is advisory), but it should be documented.
- Repair with an identical shape: same steps, and `CREATE OR REPLACE VIEW`
  trivially passes.
