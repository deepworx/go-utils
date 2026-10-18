# Event sourcing quickstart

This guide builds a small event-sourced service: one Aggregate (`Task`), one
Projection (`task_list`), one Reactor (`notify_assignee`) and one Inbox
Source (`tracker`). It boots in memory first, with no Docker, codegen or
migration tool. It then moves to Postgres without changing the domain or
the handlers.

The code below is compiled and tested in CI. It lives in
[`internal/quickstart`](../../internal/quickstart):

```
internal/quickstart/
  todo/          domain: imports only pkg/eventsourcing/aggregate
  app/           commands, queries, Projection, Reactor, HTTP API
  migrations/    the service's own migration (000002_task_list.up.sql)
  server/        the Postgres main and its end-to-end test
```

In a real service, put the domain in its own package (for example
`internal/domain/...`), and put `docs/postgres/schema.sql` into your own
`migrations/` folder as `000001_eventsourcing.up.sql` (see
[Migrations](#migrations)).

Domain terms (Tenant, Stream, Cursor, Consumer, Parked, Stopped and so on) are
used as `CONTEXT.md` defines them.

## 1. Domain

An Aggregate embeds `aggregate.Base` by value. Its zero value is a valid
starting state, because the Repository allocates it. Commands validate their
input and call `aggregate.Raise`. Raise runs the same `Apply` that rebuilds
state from history.

```go
package todo

type TaskID uuid.UUID

type TaskOpened struct {
	Title    string `json:"title"`
	Assignee string `json:"assignee"`
}

func (TaskOpened) EventType() string { return "todo.task.opened" }

type TaskCompleted struct{}

func (TaskCompleted) EventType() string { return "todo.task.completed" }

type AssigneeNotified struct{}

func (AssigneeNotified) EventType() string { return "todo.task.assignee_notified" }

type Task struct {
	aggregate.Base[TaskID]
	opened   bool
	done     bool
	notified bool
}

func (*Task) StreamType() string { return "todo.task" }

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

func (t *Task) Complete() error {
	if t.done {
		return ErrAlreadyDone
	}
	aggregate.Raise(t, TaskCompleted{})
	return nil
}

// MarkNotified is idempotent, because a Reactor may deliver an event twice.
func (t *Task) MarkNotified() {
	if !t.notified {
		aggregate.Raise(t, AssigneeNotified{})
	}
}
```

Rules that never relax:

- An Event type and a stream type never change once events have been
  written.
- Events are stored as the JSON of the struct itself, so every exported
  field needs a `json` tag
  ([ADR-0001](../adr/0001-json-payloads-stored-as-domain-events.md)).
- `Apply` must not fail and must have no side effects.

Test the domain without any store, using `aggregatetest`:

```go
task := aggregatetest.New[todo.TaskID, todo.Task](id, todo.TaskOpened{Title: "x"}, todo.TaskCompleted{})
err := task.Complete()             // todo.ErrAlreadyDone
raised := aggregatetest.Raised(task) // nil
```

### Keeping the domain pure

Let golangci-lint (v2) enforce that domain packages import only the
aggregate package, the stdlib and uuid. This repo lints
`internal/quickstart/todo` with the same rule:

```yaml
linters:
  enable:
    - depguard
  settings:
    depguard:
      rules:
        domain:
          list-mode: strict
          files:
            - "**/internal/domain/**"
            - "!$test"
          allow:
            - $gostd
            - github.com/google/uuid
            - github.com/deepworx/go-utils/pkg/eventsourcing/aggregate
```

Leave out `"!$test"` if domain tests may import nothing else either. Tests
that use `aggregatetest` need it.

## 2. Codec

Each service owns a Codec. There is no global registry.

```go
func Codec() *eventstore.Codec {
	c := eventstore.NewCodec()
	eventstore.Register[todo.TaskOpened](c)
	eventstore.Register[todo.TaskCompleted](c)
	eventstore.Register[todo.AssigneeNotified](c)
	return c
}

func TestCodec(t *testing.T) { eventstore.CheckCodec(t, app.Codec()) }
```

`CheckCodec` fails on exported fields without a `json` tag and on lossy round
trips. To change an event's shape, register an Upcaster. That raises the
event's Schema version by one, and stored events are upcast on every read:

```go
eventstore.Register[todo.TaskOpened](c, eventstore.UpcastObject(func(m map[string]any) error {
	m["assignee"] = m["owner"]
	delete(m, "owner")
	return nil
}))
```

## 3. Commands

A Repository runs each command in one Command transaction on one Stream:

```go
type Tasks = eventstore.Repository[todo.TaskID, todo.Task, *todo.Task]

func (a *App) OpenTask(ctx context.Context, in OpenTask) (todo.TaskID, eventstore.CommandResult, error) {
	v7, err := uuid.NewV7()
	if err != nil {
		return todo.TaskID{}, eventstore.CommandResult{}, fmt.Errorf("task id: %w", err)
	}
	id := todo.TaskID(v7)
	res, err := a.tasks.Create(ctx, id, func(_ context.Context, t *todo.Task) error {
		return t.Open(in.Title, in.Assignee)
	})
	...
}

func (a *App) CompleteTask(ctx context.Context, id todo.TaskID) (eventstore.CommandResult, error) {
	res, err := a.tasks.Update(ctx, id, func(_ context.Context, t *todo.Task) error {
		return t.Complete()
	})
	...
}
```

- `CommandResult.Position` is the Cursor of the last event written. Clients
  send it back for read-your-writes (see [Read-your-writes](#read-your-writes)).
- `CommandResult.Version` is the Aggregate version. A client that sends it
  with its next command (`eventstore.ExpectVersion(v)`) gets
  `ErrConcurrencyConflict` instead of acting on a stale view.
- A client that may resend a command after a lost response passes
  `eventstore.WithCommandID(id)` with a UUID it chose. A repeat returns the
  original result with `Replayed` set and does not run `fn` again
  (ADR-0012). `eventstore.CommandCleanupJob(store, retention)` deletes
  Command IDs daily (default retention 30 days); a repeat after that runs
  as a new command, so clients must not resend older commands.
- Writers to one Stream are serialized from the load until the commit. On
  Postgres this uses a transaction-scoped advisory lock.
- On Postgres, Command transactions must run at READ COMMITTED, including an
  outer transaction they join. Any other isolation level fails with
  `eventstore.ErrUnsupportedIsolation` (ADR-0013).
- Guard (`pkg/postgres/guard`) and numbering (`pkg/postgres/numbering`) calls
  inside `fn` join the Command transaction. The stream lock serializes only
  writers to this Stream, so reads of other data are as fresh as READ
  COMMITTED, no more.

### Request metadata: Tenant, Actor, Correlation

Every Repository call needs a Tenant in ctx (`eventstore.ErrNoTenant`). Every
append needs an Actor (`eventstore.ErrNoActor`). A single-tenant service sets
a fixed Tenant once, in middleware or on the root context
([ADR-0002](../adr/0002-tenant-column-with-global-consumers.md)):

```go
func Metadata(tenant string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get("X-User")
		if user == "" {
			http.Error(w, "missing X-User", http.StatusUnauthorized)
			return
		}
		ctx := ctxutil.WithTenantID(r.Context(), tenant)
		ctx = ctxutil.WithActor(ctx, "user:"+user)
		if id := r.Header.Get("X-Request-ID"); id != "" {
			ctx = ctxutil.WithRequestID(ctx, id)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
```

`X-User` stands in for real authentication. Behind
`pkg/connectrpc/jwtauth` you need none of this: `Claims.TenantID` is the
Tenant and `Claims.UserID` is the Actor. An explicit `ctxutil.WithTenantID`
still wins over the claims, which is how a single-tenant service pins its
Tenant.

Every event records ([ADR-0010](../adr/0010-event-metadata-actor-correlation-causation.md)):

- **Actor:** `ctxutil.WithActor`, else `Claims.UserID`. The prefix `system:`
  is reserved for library components (`system:reactor:<name>`,
  `system:scheduler:<job>`, `system:inbox:<source>`).
- **Correlation:** `ctxutil.WithCorrelationID`, else the request ID
  (`pkg/connectrpc/requestid` sets it), else a UUIDv7 per Command
  transaction.
- **Causation:** set automatically inside a Reactor.

## 4. Projection

A Projection builds a read model inside its Cursor transaction. The handler is
a `consumer.Router`, and its routes also form the Consumer's event-type filter
([ADR-0006](../adr/0006-consumers-filter-by-event-type.md)). The quickstart
writes through a small `ReadModel` interface. Its in-memory implementation
uses `postgres.Stage` and its Postgres implementation uses `postgres.Conn`, so
the handler is the same in both boots:

```go
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
	...
}
```

```go
// Postgres: joins the Cursor transaction in ctx.
func (m *PgReadModel) SetDone(ctx context.Context, tenant string, id uuid.UUID) error {
	_, err := postgres.Conn(ctx, m.pool).Exec(ctx,
		`UPDATE task_list_v1 SET done = true WHERE tenant_id = $1 AND id = $2`, tenant, id)
	...
}

// In memory: applied when the Cursor transaction commits, dropped on rollback.
func (m *InMemoryReadModel) Insert(ctx context.Context, tenant string, task Task) error {
	postgres.Stage(ctx, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.tasks[tenant] = append(m.tasks[tenant], task)
	})
	return nil
}
```

With sqlc, pass `postgres.Conn(ctx, pool)` to `New`: it returns the Cursor
transaction inside a handler, and the pool outside one.

```go
taskList, err := projection.New(rt, projection.Config{
	Name: "task_list",
	Active: projection.Version{
		N:       1,
		Handler: TaskListV1(codec, readModel),
		Clear:   readModel.Clear, // only projection.Reset needs it
	},
})
```

Version N runs as the Consumer `task_list@vN` and writes its own tables,
`task_list_vN`. The service's migration owns the tables and a view that
readers query ([ADR-0005](../adr/0005-projection-versions-switched-by-service-migrations.md)).
The library never runs DDL:

```sql
-- migrations/000002_task_list.up.sql
CREATE TABLE task_list_v1 (
    tenant_id TEXT        NOT NULL,
    id        UUID        NOT NULL,
    title     TEXT        NOT NULL,
    assignee  TEXT        NOT NULL,
    done      BOOLEAN     NOT NULL DEFAULT false,
    notified  BOOLEAN     NOT NULL DEFAULT false,
    opened_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id)
);

CREATE VIEW task_list AS
    SELECT tenant_id, id, title, assignee, done, notified, opened_at FROM task_list_v1;
```

A Projection always runs Transactional, so each event takes effect exactly
once. A failing event is retried and then stops the Consumer (Stopped). It is
never skipped. See [Operations](#operations).

### Change notifications

`projection.Notify(ctx, entity, id)` sends a `projection.Change{Tenant, Entity, ID}`
when the Cursor transaction commits. On Postgres it goes out on the channel
`<schema>.changes`. A live UI subscribes per Runtime:

```go
sub, err := projection.Subscribe(ctx, rt) // PgRuntime: needs its Listener
defer sub.Close()
for c := range sub.C() {
	if c.Gap {
		// changes may have been lost: refetch everything shown
		continue
	}
	if c.Tenant != viewerTenant {
		continue // every Tenant's changes arrive here
	}
}
```

A Subscription receives the changes of every Tenant (ADR-0014). Filter by
`Change.Tenant` before forwarding a change anywhere a Tenant can see it.

Subscribe returns before the LISTEN takes effect, so fetch the current state
after subscribing, not before.

## 5. Reactor

A Reactor turns events into commands
([ADR-0007](../adr/0007-reactors-park-streams-and-redrive-them.md)). It is
stateless: any state it needs lives in Aggregates. Delivery is at least
once, so `env.EventID` is the idempotency key for outbound calls, and the
follow-up command relies on the Aggregate's own guard (`MarkNotified` raises
nothing the second time):

```go
func NotifyAssignee(codec *eventstore.Codec, tasks *Tasks, mailer Mailer) *consumer.Router {
	r := consumer.NewRouter(codec)
	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, e todo.TaskOpened) error {
		if e.Assignee == "" {
			return nil
		}
		err := mailer.Send(ctx, Mail{
			To:             e.Assignee,
			Subject:        "New task: " + e.Title,
			IdempotencyKey: env.EventID.String(),
		})
		if errors.Is(err, ErrInvalidAddress) {
			return consumer.Permanent(err)
		}
		if err != nil {
			return fmt.Errorf("mail assignee: %w", err)
		}
		_, err = tasks.Update(ctx, todo.TaskID(env.StreamID), func(_ context.Context, t *todo.Task) error {
			t.MarkNotified()
			return nil
		})
		return err
	})
	return r
}

notifier, err := reactor.New(rt, reactor.Config{
	Name:    "notify_assignee",
	Handler: NotifyAssignee(codec, tasks, mailer),
	StartAt: reactor.FromBeginning,
})
```

- The handler ctx carries the event's Tenant and Correlation, the Actor
  `system:reactor:notify_assignee`, and the event as Causation. The follow-up
  command records all of them without any code.
- `StartAt` is required. `FromNow` seeds the Cursor at the head of the log on
  the Reactor's first claim, so it never reacts to history. Use it for a
  Reactor added to a service that already has events. A Reactor that ships
  with the service's first release uses `FromBeginning`. With `FromNow`, the
  events written between the first deploy and the first claim would be
  skipped.
- A reaction that fails after its retries (`reactor.WithRetry`, default 5
  attempts), or at once with `consumer.Permanent`, a panic or an undecodable
  event, Parks its Stream. Later events of that Stream are held in order while
  the Cursor moves on for every other Stream. The lease holder redrives Parked
  Streams automatically on the `reactor.WithRedrive` schedule (default 1m, 5m,
  30m, 2h, 12h). After the last step the Stream is exhausted. See
  [Operations](#operations).

## 6. Boot in memory

For prototypes and app-service tests: no Postgres, no Docker. This is the
body of `TestInMemoryBoot` in `internal/quickstart/app`:

```go
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

g, ctx := errgroup.WithContext(ctx)
g.Go(func() error { return a.Consumers().Run(ctx) }) // Projection + Reactor

ctx = ctxutil.WithTenantID(ctx, "default")
ctx = ctxutil.WithActor(ctx, "user:alice")
id, res, err := a.OpenTask(ctx, app.OpenTask{Title: "Write quickstart", Assignee: "alice@example.com"})
tasks, fresh, err := a.ListTasks(ctx, res.Position) // fresh == consumer.Fresh
```

`app.New` builds the Repository, the Projection and the Reactor from the
Config. `a.Consumers()` returns the `consumer.Group` of all of them:

```go
func (a *App) Consumers() consumer.Group {
	return append(consumer.Group(a.taskList.Consumers()), a.notifier.Consumer())
}
```

The in-memory store behaves like Postgres where it matters:

- Events are encoded with the Codec and upcast on load.
- Appends become visible only when the Command transaction commits.
- A Consumer reads only below the oldest open Command transaction.

For deterministic tests, skip `Run` and call `Drain` on each Consumer (or
`Reactor.Drain`). Drain processes everything up to the head of the log at
the time of the call.

## 7. Boot on Postgres

### Migrations

The library ships its schema as documentation, not as a migrator
([ADR-0003](../adr/0003-library-schema-shipped-as-documentation-only.md)).
With golang-migrate:

1. Copy `docs/postgres/schema.sql` of the go-utils version you depend on to
   `migrations/000001_eventsourcing.up.sql`. Delete the sections of packages
   you don't use (`guards`, `identifier_sequences`, `scheduler_*`,
   `inbox_messages`, `parked_*`). Keep the PostgreSQL 15 guard at the top,
   and keep every constraint of the tables you use under its name, above
   all `events_stream_position_key`.
2. Put the service's own tables and views in the next migrations
   (`000002_task_list.up.sql`).
3. Apply with `migrate -path migrations -database "$DATABASE_URL" up`. Do not
   set `x-multi-statement`: it splits on `;` and breaks the `DO $$ … $$` and
   function bodies. Without it, each file runs as one multi-statement Exec.

```sh
cp "$(go env GOMODCACHE)/github.com/deepworx/go-utils@vX.Y.Z/docs/postgres/schema.sql" \
   migrations/000001_eventsourcing.up.sql
```

**Upgrades.** Every later change to the library schema ships as a snippet
`docs/postgres/upgrades/NNNN_<description>.sql`. The commit carries a
`BREAKING CHANGE:` footer that names the snippet. When you bump go-utils, copy
each new snippet 1:1 into your next migration and apply it before the new pods
roll out. Snippets stay compatible with the previous minor version, so old pods
keep working during the rollout. There is no runtime schema check: new code on
an old schema fails on its first query. (This release has no snippets yet.
`schema.sql` is the complete state.)

**Non-public schema.** `schema.sql` is written for `public`. To keep the
library tables in their own schema, for example `todo`, create the schema and
replace every `public.` qualifier. This also renames the hard-coded
`public.inbox` notify channel. The events trigger derives its channel from
the table's schema:

```sh
{ echo 'CREATE SCHEMA IF NOT EXISTS todo;'
  sed 's/public\./todo./g' schema.sql
} > migrations/000001_eventsourcing.up.sql
```

Apply the same `sed` to every upgrade snippet. Then pass the schema to every
`Pg*` constructor that takes one:

- `eventstore.PgStoreConfig.Schema`, and the same value in
  `eventstore.PgUnitOfWorkConfig.Schema`
- `scheduler.Config.Schema`
- `inbox.StoreConfig.Schema`

`consumer.PgRuntime` takes the schema from its Store. The schema derives the
notify channels `<schema>.events`, `<schema>.changes` and `<schema>.inbox`,
and the lease namespaces `<schema>.consumer`, `<schema>.scheduler` and
`<schema>.inbox`. That makes the name limit 56 bytes, which the constructors
check. The quickstart's end-to-end test migrates exactly this way and runs
the service with the library tables in `todo`.

### main

One `notify.Listener` and one `lease.Claims` serve every component.
Everything runs in one errgroup next to the HTTP server, so the first
failure, or SIGTERM, stops it all. This is `serve` from
`internal/quickstart/server/main.go`:

```go
func serve(ctx context.Context, in serveInput) error {
	codec := app.Codec()
	store, err := eventstore.NewPgStore(eventstore.PgStoreConfig{Pool: in.Pool, Codec: codec, Schema: in.Schema})
	if err != nil {
		return fmt.Errorf("event store: %w", err)
	}
	uow, err := eventstore.NewPgUnitOfWork(eventstore.PgUnitOfWorkConfig{
		UnitOfWork: postgres.NewUnitOfWork(in.Pool),
		Schema:     in.Schema,
	})
	if err != nil {
		return fmt.Errorf("unit of work: %w", err)
	}
	listener := notify.NewListener(in.Pool, notify.Config{})
	claims := lease.NewClaims(in.Pool, lease.Config{})
	rt, err := consumer.NewPgRuntime(consumer.PgRuntimeConfig{
		Pool:     in.Pool,
		Store:    store,
		Listener: listener,
		Claims:   claims,
	})
	if err != nil {
		return fmt.Errorf("consumer runtime: %w", err)
	}
	a, err := app.New(app.Config{
		Codec:      codec,
		Store:      store,
		UnitOfWork: uow,
		Runtime:    rt,
		ReadModel:  app.NewPgReadModel(in.Pool),
		Mailer:     in.Mailer,
	})
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}

	inboxStore, err := inbox.NewStore(in.Pool, inbox.StoreConfig{Schema: in.Schema})
	if err != nil {
		return fmt.Errorf("inbox: %w", err)
	}
	tracker, err := inbox.NewWorker(inboxStore, inbox.Config{
		Source: app.TrackerSource,
		Handle: a.HandleTrackerWebhook,
	}, inbox.WithListener(listener), inbox.WithClaims(claims))
	if err != nil {
		return fmt.Errorf("tracker worker: %w", err)
	}

	sched, err := scheduler.New(scheduler.Config{Pool: in.Pool, Claims: claims, Schema: in.Schema, Location: time.UTC})
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	if err := sched.Register(inbox.CleanupJob(inboxStore, 0)); err != nil {
		return fmt.Errorf("register inbox cleanup: %w", err)
	}
	if err := sched.Register(eventstore.CommandCleanupJob(store, 0)); err != nil {
		return fmt.Errorf("register command cleanup: %w", err)
	}

	mux := http.NewServeMux()
	a.Routes(mux)
	mux.Handle("POST /webhooks/tracker", app.ReceiveTrackerWebhook(inboxStore))
	srv := &http.Server{
		Handler:           app.Metadata(in.Tenant, mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return listener.Run(ctx) })
	g.Go(func() error { return claims.Run(ctx) })
	g.Go(func() error { return a.Consumers().Run(ctx) })
	g.Go(func() error { return tracker.Run(ctx) })
	g.Go(func() error { return sched.Run(ctx) })
	g.Go(func() error {
		if err := srv.Serve(in.Listener); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
	return g.Wait()
}
```

`run` in the same file does the process setup: `signal.NotifyContext`,
`otel.Setup` (global providers, native histograms), `postgres.NewPool` and
`shutdown.Shutdown` on exit.

Notes on the wiring:

- `Claims.Run` must be running for Consumers, the scheduler and the inbox
  gauges. Each Consumer runs under its own lease
  ([ADR-0011](../adr/0011-leases-are-not-fences.md)), so every pod can run
  the same Group and exactly one of them processes each Consumer. Leases
  only prevent duplicate work. Correctness comes from the compare-and-set
  on the Cursor, and from the claim fences of the scheduler and the inbox.
- Without a Listener, Consumers and Workers only poll (default every 1s).
- The Store and UnitOfWork must be a matching pair: `PgStore` with
  `PgUnitOfWork`, or `InMemoryStore` with `InMemoryUnitOfWork`.
- For Postgres row-level security, build the UnitOfWork with
  `postgres.NewUnitOfWork(pool, postgres.SetLocalTenant())`. Every Command
  transaction then sets `app.tenant_id`.
- Spans and metrics use the OTel globals that `otel.Setup` installs
  ([ADR-0004](../adr/0004-observability-across-the-event-log.md)). Consumer
  spans link to the command that wrote the event.

### Read-your-writes

A Consumer is asynchronous, so a client that reads right after its command
may not see its own write. The command returns its position. The quickstart
encodes it as `<transaction>.<position>`
(`app.FormatPosition`/`app.ParsePosition`), and the client sends it back with
its next read:

```go
func (a *App) ListTasks(ctx context.Context, after eventstore.Cursor) ([]Task, consumer.Freshness, error) {
	tenant, ok := ctxutil.TenantID(ctx)
	if !ok {
		return nil, 0, eventstore.ErrNoTenant
	}
	fresh := a.taskList.Await(ctx, after)
	tasks, err := a.readModel.List(ctx, tenant)
	...
	return tasks, fresh, nil
}
```

`Await` waits until the active Projection version has caught up to `after`,
for at most the ctx deadline or else 2s. It returns:

- `Fresh` when the version has caught up, or at once for a zero position.
- `Stale` when the budget ran out.
- `StaleStopped` when the Consumer is Stopped short of the position.

`Stale` is not an error. Serve the data and tell the client
(`"freshness": "Stale"`). All Awaits on one Consumer share one poller.

### Inbox and scheduler

The Inbox is a deduplicated work queue for incoming webhooks, independent of
the event log. The HTTP handler only stores the message and answers 202. The
Worker of its Source handles it later, with retries:

```go
_, err = store.Insert(r.Context(), inbox.Message{
	Source:  TrackerSource,
	Key:     hook.DeliveryID, // a redelivered webhook is dropped
	Type:    "task.closed",
	Tenant:  tenant,
	Payload: body,
})
```

```go
func (a *App) HandleTrackerWebhook(ctx context.Context, d inbox.Delivery) error {
	var hook TrackerWebhook
	if err := json.Unmarshal(d.Payload, &hook); err != nil {
		return inbox.Permanent(fmt.Errorf("decode tracker webhook: %w", err))
	}
	_, err := a.CompleteTask(ctx, todo.TaskID(hook.TaskID))
	switch {
	case errors.Is(err, todo.ErrAlreadyDone):
		return inbox.Ignore("task already done")
	case errors.Is(err, eventstore.ErrNotFound):
		return inbox.Permanent(err)
	}
	return err
}
```

The handler ctx carries the message's Tenant, the Actor
`system:inbox:tracker` and the Correlation `inbox:tracker:<key>`. `Insert`
joins a transaction in ctx, so a command can store a message atomically with
its own writes.

The scheduler runs jobs at most once per Business date (daily jobs) or per
grid point (`Job.Every`), and never catches up on missed runs
([ADR-0008](../adr/0008-scheduler-runs-what-is-due-once-and-never-catches-up.md)).
`Config.Location` has no default, so choose the business timezone
explicitly. `inbox.CleanupJob(store, retention)` deletes settled messages
daily (default retention 7 days). A Scheduler holds one CleanupJob only,
because its name is fixed. A job's ctx has the Actor
`system:scheduler:<job>` and a fresh Correlation. Jobs must be idempotent
for their date or grid point, because an aborted run is retried after
`RetryAfter`.

## 8. Operations

**Stopped Consumers.** A Projection event that keeps failing stops its
Consumer at its Cursor. It reports `Status().Stopped` and the
`eventsourcing.consumer.stopped` gauge. Fix the cause, deploy, then call
`rt.Resume(ctx, "task_list@v1")`. On Postgres, Resume works from any process,
for example an admin endpoint. A Projection never skips an event.

**Parked Streams.** `notifier.Parked(ctx, tenant)` lists the held events.
`Redrive(ctx, reactor.Stream{Tenant, ID})` replays them in order.
`Discard(ctx, stream, eventID)` skips one held event; it needs an Actor in
ctx, which is recorded. `Status` and the `eventsourcing.reactor.parked.*`
gauges count Parked Streams, held events and exhausted Streams.

**Inbox.** `inboxStore.List(ctx, inbox.Filter{Status: inbox.StatusParked})`,
`Redrive(ctx, id)` and `Discard(ctx, id, reason)`. Discard needs an Actor in
ctx, which is recorded. Parked messages are never cleaned up.

**Operator calls are cross-tenant.** The calls above take their tenant from
their arguments, never from ctx, and do no authorization (ADR-0014).
`List` returns every Tenant's payloads, `Parked(ctx, "")` lists every Tenant,
and an unknown inbox ID fails differently from a known one. Expose them only
to Operators, or guard them in the app before a tenant admin can call them.

**Long transactions delay every Consumer (pg_snapshot_xmin is
cluster-wide).** A Postgres Consumer reads only events below
`pg_snapshot_xmin(pg_current_snapshot())`. That way it never skips an event
of a transaction that commits later with a smaller Cursor. The xmin horizon
is cluster-wide: any open transaction in **any database on the same Postgres
cluster** holds it back. This includes an analytics query, a session left
`idle in transaction`, a long migration, or a `pg_dump`. While it is open,
every Consumer of every service on that cluster stops seeing new events:

- Projections fall behind.
- `Await` returns `Stale`.
- `Drain` sees nothing new, because it does not wait for visibility.

No event is lost. Everything catches up once the transaction ends.

To limit the impact:

- Set `idle_in_transaction_session_timeout`.
- Bound batch jobs with `statement_timeout`.
- Keep Command and Cursor transactions short.
- Alert on the `eventsourcing.xmin.age` gauge (in transactions) and on
  `eventsourcing.consumer.pending` and `eventsourcing.consumer.lag`.
- Don't share the cluster with workloads that hold long transactions.

The same applies to tests: parallel Postgres tests in other databases of one
container hold back each other's Consumers. Run Postgres Consumer tests
sequentially.

**Append-only log.** The events table rejects UPDATE, DELETE and TRUNCATE
([ADR-0009](../adr/0009-event-log-is-append-only-and-tenant-data-is-not-erased.md)).
Fix data with new events or a new Projection version, never in place.

## 9. Bumping a Projection version

Changing a read model's shape, or repairing its data, is a version bump
([ADR-0005](../adr/0005-projection-versions-switched-by-service-migrations.md)).
It spans two releases plus a cleanup. Leave `projection.Config.EventTypes`
empty, so each version takes its filter from its own Router, and a new version
can handle more event types than the active one
([ADR-0006](../adr/0006-consumers-filter-by-event-type.md)).

**Release N: build the shadow.** A migration creates the new table:

```sql
-- migrations/000007_task_list_v2.up.sql
CREATE TABLE task_list_v2 (
    tenant_id TEXT        NOT NULL,
    id        UUID        NOT NULL,
    title     TEXT        NOT NULL,
    assignee  TEXT        NOT NULL,
    done      BOOLEAN     NOT NULL DEFAULT false,
    notified  BOOLEAN     NOT NULL DEFAULT false,
    opened_at TIMESTAMPTZ NOT NULL,
    due_on    DATE,                         -- new column, appended
    PRIMARY KEY (tenant_id, id)
);
```

```go
projection.Config{
	Name:   "task_list",
	Active: projection.Version{N: 1, Handler: TaskListV1(codec, rm), Clear: rm.Clear},
	Shadow: &projection.Version{N: 2, Handler: TaskListV2(codec, rmV2), Clear: rmV2.Clear},
}
```

- `task_list@v2` builds from the first event while readers, `Await` and
  `Notify` stay on v1. In the shadow, `Notify` does nothing.
- `CaughtUp(ctx)` reports when the shadow has caught up. You can also watch
  `eventsourcing.consumer.pending{eventsourcing.consumer="task_list@v2"}`.

**Release N+1: switch.** Once `CaughtUp` is true, a migration switches the
view. A guard makes it fail while the shadow still lags:

```sql
-- migrations/000008_task_list_switch.up.sql
DO $$
BEGIN
    IF public.consumer_pending('task_list@v2', 100) > 100 THEN
        RAISE EXCEPTION 'projection task_list@v2 is not caught up';
    END IF;
END
$$;
CREATE OR REPLACE VIEW task_list AS
    SELECT tenant_id, id, title, assignee, done, notified, opened_at, due_on FROM task_list_v2;
```

```go
projection.Config{
	Name:   "task_list",
	Active: projection.Version{N: 2, Handler: TaskListV2(codec, rmV2), Clear: rmV2.Clear},
}
```

- The threshold (100) bounds how many events v2 may still have to process
  when the view switches.
- `consumer_pending` raises by itself while the shadow is Stopped or has no
  Cursor, for example after a typo in the name. A failed guard fails the
  migration; retry the rollout later.
- Use the library schema's qualifier (`todo.consumer_pending` for a
  non-public schema).
- `CREATE OR REPLACE VIEW` only accepts a view that keeps every column with
  its name, type and position, and adds new columns at the end. Pods of
  release N still read those columns during the rollout. To remove or retype
  a column, do one bump that stops reading it and a later one that drops it.
  Never switch with `DROP VIEW`.
- During the rollout, pods of release N still `Await` v1 while the view
  already shows v2. Their reads may be briefly stale. That is acceptable,
  because Freshness is advisory.

**Release N+2: clean up.**

```sql
DROP TABLE task_list_v1;
DELETE FROM public.consumer_cursors WHERE name = 'task_list@v1';
```

`projection.Reset` (Clear plus rewinding every version's Cursor) is for
prototypes and development only. In production, repair a read model with a
version bump.

## Decisions behind this design

| ADR | Decision |
|-----|----------|
| [0001](../adr/0001-json-payloads-stored-as-domain-events.md) | JSON payloads, stored as the domain events themselves |
| [0002](../adr/0002-tenant-column-with-global-consumers.md) | Tenant column with per-tenant stream identity and global consumers |
| [0003](../adr/0003-library-schema-shipped-as-documentation-only.md) | Library schema shipped as documentation only |
| [0004](../adr/0004-observability-across-the-event-log.md) | Observability across the event log |
| [0005](../adr/0005-projection-versions-switched-by-service-migrations.md) | Projection versions are switched by the service's migrations |
| [0006](../adr/0006-consumers-filter-by-event-type.md) | Consumers filter the log by event type |
| [0007](../adr/0007-reactors-park-streams-and-redrive-them.md) | Reactors park streams and redrive them on a schedule |
| [0008](../adr/0008-scheduler-runs-what-is-due-once-and-never-catches-up.md) | The scheduler runs what is due once and never catches up |
| [0009](../adr/0009-event-log-is-append-only-and-tenant-data-is-not-erased.md) | The event log is append-only and tenant data is not erased |
| [0010](../adr/0010-event-metadata-actor-correlation-causation.md) | Event metadata: actor, correlation and causation |
| [0011](../adr/0011-leases-are-not-fences.md) | Leases are not fences: holders fence their own writes |
