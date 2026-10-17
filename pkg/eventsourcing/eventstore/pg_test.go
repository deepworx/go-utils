package eventstore_test

import (
	"context"
	"errors"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/internal/eventstoretest"
	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/guard"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
	"github.com/deepworx/go-utils/pkg/postgres/numbering"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

func TestPgContract(t *testing.T) {
	t.Parallel()
	eventstoretest.Run(t, func(t *testing.T) eventstoretest.Backend {
		pool := pgtest.New(t)
		return eventstoretest.Backend{
			Store: func(cfg eventstoretest.StoreConfig) eventstore.Store {
				return newPgStore(t, eventstore.PgStoreConfig{Pool: pool, Codec: cfg.Codec}, eventstore.WithClock(cfg.Clock))
			},
			UnitOfWork: newPgUnitOfWork(t, pool, ""),
			Guard:      guard.NewPgStore(pool),
			Numbering:  numbering.NewPgStore(pool),
			Outer:      postgres.NewUnitOfWork(pool).Execute,
		}
	})
}

func TestPgEventsRejectChanges(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	tasks := newTaskRepository(t, pool, "public")
	if _, err := tasks.Create(userCtx("t1"), newTaskID(), createTask("a")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, stmt := range []string{
		"UPDATE public.events SET actor = 'mallory'",
		"DELETE FROM public.events",
		"TRUNCATE public.events",
	} {
		if _, err := pool.Exec(t.Context(), stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want append-only rejection", stmt, err)
		}
	}
	if n := countEvents(t, pool, "public"); n != 1 {
		t.Fatalf("events after rejected changes = %d, want 1", n)
	}
}

func TestPgUnitOfWorkLocksPerTenantAndStream(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	uow := newPgUnitOfWork(t, pool, "")
	ctx := ctxutil.WithTenantID(context.Background(), "t1")
	stream := uuid.New()
	holdStream(t, uow, ctx, stream)

	tests := []struct {
		name   string
		ctx    context.Context
		stream uuid.UUID
		want   error
	}{
		{"same stream waits until ctx ends", timeoutCtx(t, ctx), stream, context.DeadlineExceeded},
		{"same stream id under another tenant does not wait", boundedCtx(t, ctxutil.WithTenantID(ctx, "t2")), stream, nil},
		{"other stream does not wait", boundedCtx(t, ctx), uuid.New(), nil},
	}
	for _, tt := range tests {
		err := uow.Execute(tt.ctx, tt.stream, func(context.Context) error { return nil })
		if !errors.Is(err, tt.want) {
			t.Fatalf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestPgNonPublicSchemaUsesDerivedNames(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	applySchemaAs(t, pool, "alt")
	ctx := userCtx("t1")

	listener, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(listener.Release)
	if _, err := listener.Exec(t.Context(), `LISTEN "alt.events"`); err != nil {
		t.Fatalf("listen: %v", err)
	}

	tasks := newTaskRepository(t, pool, "alt")
	id := newTaskID()
	if _, err := tasks.Create(ctx, id, createTask("a")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("writes go to the schema's events table", func(t *testing.T) {
		if got := countEvents(t, pool, "alt"); got != 1 {
			t.Errorf("alt.events = %d rows, want 1", got)
		}
		if got := countEvents(t, pool, "public"); got != 0 {
			t.Errorf("public.events = %d rows, want 0", got)
		}
	})

	t.Run("notify uses the schema's channel", func(t *testing.T) {
		wctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		n, err := listener.Conn().WaitForNotification(wctx)
		if err != nil {
			t.Fatalf("wait for notification: %v", err)
		}
		if n.Channel != "alt.events" || n.Payload != "" {
			t.Fatalf("notification = %q %q, want alt.events without payload", n.Channel, n.Payload)
		}
	})

	t.Run("stream lock key includes the schema", func(t *testing.T) {
		stream := uuid.UUID(id)
		holdStream(t, newPgUnitOfWork(t, pool, "alt"), ctx, stream)

		if !advisoryLockHeld(t, pool, fnv64a("alt\x00t1\x00"+stream.String())) {
			t.Fatal("no advisory lock with key fnv64a(alt‖0‖t1‖0‖stream)")
		}
		public := newPgUnitOfWork(t, pool, "public")
		if err := public.Execute(boundedCtx(t, ctx), stream, func(context.Context) error { return nil }); err != nil {
			t.Fatalf("same stream in public: %v, want no wait", err)
		}
	})
}

func TestPgConstructorsRejectMissingDependencies(t *testing.T) {
	t.Parallel()
	pool := &pgxpool.Pool{}
	tests := []struct {
		name string
		new  func() error
	}{
		{"store without pool", func() error {
			_, err := eventstore.NewPgStore(eventstore.PgStoreConfig{Codec: eventstore.NewCodec()})
			return err
		}},
		{"store without codec", func() error {
			_, err := eventstore.NewPgStore(eventstore.PgStoreConfig{Pool: pool})
			return err
		}},
		{"unit of work without postgres unit of work", func() error {
			_, err := eventstore.NewPgUnitOfWork(eventstore.PgUnitOfWorkConfig{})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.new(); !errors.Is(err, eventstore.ErrInvalidConfig) {
				t.Errorf("err = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestPgConstructorsRejectSchemaWithTooLongChannel(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("s", 57) // "<schema>.events" exceeds 63 bytes
	pool := &pgxpool.Pool{}

	_, err := eventstore.NewPgStore(eventstore.PgStoreConfig{Pool: pool, Codec: eventstore.NewCodec(), Schema: long})
	if !errors.Is(err, notify.ErrInvalidChannel) {
		t.Errorf("NewPgStore: err = %v, want ErrInvalidChannel", err)
	}
	_, err = eventstore.NewPgUnitOfWork(eventstore.PgUnitOfWorkConfig{UnitOfWork: postgres.NewUnitOfWork(pool), Schema: long})
	if !errors.Is(err, notify.ErrInvalidChannel) {
		t.Errorf("NewPgUnitOfWork: err = %v, want ErrInvalidChannel", err)
	}
	if _, err := eventstore.NewPgStore(eventstore.PgStoreConfig{Pool: pool, Codec: eventstore.NewCodec(), Schema: long[:56]}); err != nil {
		t.Errorf("NewPgStore with a 56-byte schema: %v", err)
	}
}

type taskID uuid.UUID

func newTaskID() taskID { return taskID(uuid.Must(uuid.NewV7())) }

type task struct {
	aggregate.Base[taskID]
	title string
}

func (*task) StreamType() string { return "Test.Task" }

func (t *task) Apply(e aggregate.Event) {
	if e, ok := e.(taskCreated); ok {
		t.title = e.Title
	}
}

func createTask(title string) func(context.Context, *task) error {
	return func(_ context.Context, t *task) error {
		aggregate.Raise(t, taskCreated{Title: title})
		return nil
	}
}

func newTaskRepository(t *testing.T, pool *pgxpool.Pool, schema string) *eventstore.Repository[taskID, task, *task] {
	t.Helper()
	codec := eventstore.NewCodec()
	eventstore.Register[taskCreated](codec)
	return eventstore.NewRepository[taskID, task](eventstore.RepositoryConfig{
		Store:      newPgStore(t, eventstore.PgStoreConfig{Pool: pool, Codec: codec, Schema: schema}),
		UnitOfWork: newPgUnitOfWork(t, pool, schema),
	})
}

func newPgStore(t *testing.T, cfg eventstore.PgStoreConfig, opts ...eventstore.StoreOption) *eventstore.PgStore {
	t.Helper()
	s, err := eventstore.NewPgStore(cfg, opts...)
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	return s
}

func newPgUnitOfWork(t *testing.T, pool *pgxpool.Pool, schema string) *eventstore.PgUnitOfWork {
	t.Helper()
	u, err := eventstore.NewPgUnitOfWork(eventstore.PgUnitOfWorkConfig{UnitOfWork: postgres.NewUnitOfWork(pool), Schema: schema})
	if err != nil {
		t.Fatalf("NewPgUnitOfWork: %v", err)
	}
	return u
}

func userCtx(tenant string) context.Context {
	ctx := ctxutil.WithTenantID(context.Background(), tenant)
	return ctxutil.WithClaims(ctx, ctxutil.Claims{UserID: "user-1"})
}

// holdStream keeps a Command transaction on stream open until the test
// ends.
func holdStream(t *testing.T, uow eventstore.UnitOfWork, ctx context.Context, stream uuid.UUID) {
	t.Helper()
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- uow.Execute(ctx, stream, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("holder Execute: %v", err)
	}
	t.Cleanup(func() {
		close(release)
		if err := <-done; err != nil {
			t.Errorf("holder Execute: %v", err)
		}
	})
}

// applySchemaAs applies docs/postgres/schema.sql with every public.
// qualifier replaced by schema, as its header instructs.
func applySchemaAs(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	sql, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "docs", "postgres", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	ddl := "CREATE SCHEMA " + schema + ";\n" + strings.ReplaceAll(string(sql), "public.", schema+".")
	if _, err := pool.Exec(t.Context(), ddl); err != nil {
		t.Fatalf("apply schema.sql as %s: %v", schema, err)
	}
}

// boundedCtx fails a Command transaction that should not wait instead of
// letting it hang the test.
func boundedCtx(t *testing.T, parent context.Context) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func countEvents(t *testing.T, pool *pgxpool.Pool, schema string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+schema+".events").Scan(&n); err != nil {
		t.Fatalf("count %s.events: %v", schema, err)
	}
	return n
}

func fnv64a(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64()) //nolint:gosec // advisory lock keys reuse the bit pattern
}

// advisoryLockHeld reports whether a bigint advisory lock on key is
// granted. pg_locks splits the key into classid (high) and objid (low).
func advisoryLockHeld(t *testing.T, pool *pgxpool.Pool, key int64) bool {
	t.Helper()
	var held bool
	err := pool.QueryRow(t.Context(), `SELECT EXISTS (
		SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 1 AND granted
			AND classid::bigint = ($1::bigint >> 32) & x'FFFFFFFF'::bigint
			AND objid::bigint = $1::bigint & x'FFFFFFFF'::bigint)`, key).Scan(&held)
	if err != nil {
		t.Fatalf("query pg_locks: %v", err)
	}
	return held
}
