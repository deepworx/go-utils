package consumer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/internal/consumertest"
	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

// The Pg tests do not run in parallel: their databases share one cluster,
// and pg_snapshot_xmin is cluster-wide, so a transaction in one test would
// hide fresh events from Drain in another.

func TestPgContract(t *testing.T) {
	consumertest.Run(t, func(t *testing.T, codec *eventstore.Codec) consumertest.Backend {
		e := newPgEnv(t, pgEnvConfig{codec: codec})
		return consumertest.Backend{
			Store:      e.store,
			UnitOfWork: e.uow,
			Runtime:    e.runtime(t),
			Effects:    consumertest.NewPgEffects(t, e.pool),
			Restart:    func(t *testing.T) consumer.Runtime { return e.runtime(t) },
		}
	}, consumertest.Sequential())
}

type pgEnv struct {
	pool   *pgxpool.Pool
	schema string
	store  *eventstore.PgStore
	uow    *eventstore.PgUnitOfWork
	notes  *eventstore.Repository[noteID, note, *note]
}

type pgEnvConfig struct {
	codec  *eventstore.Codec
	schema string
}

func newPgEnv(t *testing.T, cfg pgEnvConfig) pgEnv {
	t.Helper()
	if cfg.codec == nil {
		cfg.codec = newCodec()
	}
	if cfg.schema == "" {
		cfg.schema = "public"
	}
	pool := pgtest.New(t)
	if cfg.schema != "public" {
		applySchemaAs(t, pool, cfg.schema)
	}
	store, err := eventstore.NewPgStore(eventstore.PgStoreConfig{Pool: pool, Codec: cfg.codec, Schema: cfg.schema})
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	uow, err := eventstore.NewPgUnitOfWork(eventstore.PgUnitOfWorkConfig{
		UnitOfWork: postgres.NewUnitOfWork(pool), Schema: cfg.schema,
	})
	if err != nil {
		t.Fatalf("NewPgUnitOfWork: %v", err)
	}
	return pgEnv{
		pool:   pool,
		schema: cfg.schema,
		store:  store,
		uow:    uow,
		notes:  eventstore.NewRepository[noteID, note](eventstore.RepositoryConfig{Store: store, UnitOfWork: uow}),
	}
}

// runtime starts a Runtime with its own Claims and Listener, like one pod.
func (e pgEnv) runtime(t *testing.T) *consumer.PgRuntime {
	t.Helper()
	rt, _ := e.runtimeWithListener(t)
	return rt
}

func (e pgEnv) runtimeWithListener(t *testing.T) (*consumer.PgRuntime, *notify.Listener) {
	t.Helper()
	claims := lease.NewClaims(e.pool, lease.Config{PingInterval: 50 * time.Millisecond})
	listener := notify.NewListener(e.pool, notify.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = claims.Run(ctx) })
	wg.Go(func() { _ = listener.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	rt, err := consumer.NewPgRuntime(consumer.PgRuntimeConfig{Pool: e.pool, Store: e.store, Listener: listener, Claims: claims})
	if err != nil {
		t.Fatalf("NewPgRuntime: %v", err)
	}
	return rt, listener
}

func (e pgEnv) write(t *testing.T, events ...aggregate.Event) eventstore.Cursor {
	t.Helper()
	res, err := e.notes.Create(writerCtx(), noteID(uuid.Must(uuid.NewV7())), func(_ context.Context, n *note) error {
		for _, ev := range events {
			aggregate.Raise(n, ev)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	return res.Position
}

func newPgConsumer(t *testing.T, rt consumer.Runtime, cfg consumer.Config, opts ...consumer.Option) *consumer.Consumer {
	t.Helper()
	if cfg.Mode == 0 {
		cfg.Mode = consumer.Transactional
	}
	if cfg.Handler == nil {
		cfg.Handler = consumer.HandlerFunc(noop)
	}
	opts = append([]consumer.Option{quiet, consumer.WithRetry(consumer.Retry{MaxAttempts: 1, Base: time.Millisecond, Max: time.Millisecond})}, opts...)
	c, err := consumer.New(rt, cfg, opts...)
	if err != nil {
		t.Fatalf("consumer.New: %v", err)
	}
	return c
}

func TestNewPgRuntimeRejectsMissingDependencies(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t, pgEnvConfig{})
	claims := lease.NewClaims(e.pool, lease.Config{})
	tests := []struct {
		name string
		cfg  consumer.PgRuntimeConfig
	}{
		{"no pool", consumer.PgRuntimeConfig{Store: e.store, Claims: claims}},
		{"no store", consumer.PgRuntimeConfig{Pool: e.pool, Claims: claims}},
		{"no claims", consumer.PgRuntimeConfig{Pool: e.pool, Store: e.store}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := consumer.NewPgRuntime(tt.cfg); !errors.Is(err, consumer.ErrInvalidConfig) {
				t.Errorf("NewPgRuntime = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// TestPgFilteredReadUsesTypeIndex checks that every event type is read as
// one ordered index range: event_type is an index condition, never a
// filter, even under a generic plan.
func TestPgFilteredReadUsesTypeIndex(t *testing.T) {
	e := newPgEnv(t, pgEnvConfig{})
	for range 50 {
		e.write(t, noteWritten{Text: "x"}, noteDeleted{})
	}
	ctx := t.Context()
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, stmt := range []string{
		"ANALYZE public.events",
		"SET plan_cache_mode = force_generic_plan",
		"SET enable_seqscan = off",
		"PREPARE filtered_read AS " + consumer.PgFilteredReadSQL("public"),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	rows, err := conn.Query(ctx, `EXPLAIN EXECUTE filtered_read('{Test.Note.Written,Test.Note.Deleted}', '0', 0, 100)`)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	text := strings.Join(plan, "\n")

	if regexp.MustCompile(`Filter: .*event_type`).MatchString(text) {
		t.Errorf("plan filters on event_type:\n%s", text)
	}
	if !regexp.MustCompile(`Index Cond: .*event_type = t\.event_type`).MatchString(text) {
		t.Errorf("plan has no per-type index range:\n%s", text)
	}
	if strings.Contains(text, "ANY") {
		t.Errorf("plan uses event_type = ANY:\n%s", text)
	}
}

func TestPgConsumerPending(t *testing.T) {
	e := newPgEnv(t, pgEnvConfig{})
	rt := e.runtime(t)
	written := newPgConsumer(t, rt, consumer.Config{Name: "written", EventTypes: []string{noteWritten{}.EventType()}})
	all := newPgConsumer(t, rt, consumer.Config{Name: "all"})
	for _, c := range []*consumer.Consumer{written, all} {
		if err := c.Drain(t.Context()); err != nil {
			t.Fatalf("Drain %s: %v", c.Name(), err)
		}
	}
	e.write(t, noteWritten{Text: "a"}, noteDeleted{}, noteWritten{Text: "b"}, noteWritten{Text: "c"})

	tests := []struct {
		name     string
		consumer string
		cap      int
		want     int
	}{
		{"filtered counts matching types", "written", 10, 3},
		{"filtered is capped at cap+1", "written", 1, 2},
		{"unfiltered counts every type", "all", 10, 4},
		{"unfiltered is capped at cap+1", "all", 2, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := consumerPending(t, e.pool, tt.consumer, tt.cap); got != tt.want {
				t.Errorf("consumer_pending(%s, %d) = %d, want %d", tt.consumer, tt.cap, got, tt.want)
			}
		})
	}

	t.Run("missing cursor raises", func(t *testing.T) {
		if _, err := queryPending(e.pool, "missing", 10); err == nil {
			t.Error("consumer_pending of a missing Cursor succeeded, want an error")
		}
	})
	t.Run("stopped cursor raises", func(t *testing.T) {
		stopping := newPgConsumer(t, rt, consumer.Config{
			Name: "stopping",
			Handler: consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error {
				return consumer.Permanent(errors.New("broken"))
			}),
		})
		if err := stopping.Drain(t.Context()); !errors.Is(err, consumer.ErrStopped) {
			t.Fatalf("Drain = %v, want ErrStopped", err)
		}
		_, err := queryPending(e.pool, "stopping", 10)
		if err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Errorf("consumer_pending of a Stopped Cursor = %v, want a stopped error", err)
		}
	})
}

func consumerPending(t *testing.T, pool *pgxpool.Pool, name string, cap int) int {
	t.Helper()
	n, err := queryPending(pool, name, cap)
	if err != nil {
		t.Fatalf("consumer_pending(%s, %d): %v", name, cap, err)
	}
	return n
}

func queryPending(pool *pgxpool.Pool, name string, cap int) (int, error) {
	var n int
	err := pool.QueryRow(context.Background(), "SELECT public.consumer_pending($1, $2)", name, cap).Scan(&n)
	return n, err
}

func TestPgDeliversEarlierTransactionCommittedLater(t *testing.T) {
	e := newPgEnv(t, pgEnvConfig{})
	var seen []string
	r := consumer.NewRouter(newCodec())
	consumer.On(r, func(_ context.Context, _ eventstore.Envelope, n noteWritten) error {
		seen = append(seen, n.Text)
		return nil
	})
	c := newPgConsumer(t, e.runtime(t), consumer.Config{Name: "order", Handler: r})

	early := noteID(uuid.Must(uuid.NewV7()))
	appended := make(chan struct{})
	release := make(chan struct{})
	releaseEarly := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseEarly)
	done := make(chan error, 1)
	go func() {
		done <- e.uow.Execute(writerCtx(), uuid.UUID(early), func(ctx context.Context) error {
			_, err := e.notes.Create(ctx, early, func(_ context.Context, n *note) error {
				aggregate.Raise(n, noteWritten{Text: "early"})
				return nil
			})
			close(appended)
			<-release
			return err
		})
	}()
	<-appended
	e.write(t, noteWritten{Text: "late"})

	if err := c.Drain(t.Context()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("seen %v while an earlier transaction is open, want nothing", seen)
	}
	releaseEarly()
	if err := <-done; err != nil {
		t.Fatalf("early transaction: %v", err)
	}
	if err := c.Drain(t.Context()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !slices.Equal(seen, []string{"early", "late"}) {
		t.Errorf("seen %v, want [early late]", seen)
	}
}

func TestPgDrainWhileOtherRuntimeHoldsLease(t *testing.T) {
	e := newPgEnv(t, pgEnvConfig{})
	handled := make(chan struct{}, 16)
	first := newPgConsumer(t, e.runtime(t), consumer.Config{
		Name: "shared",
		Handler: consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error {
			handled <- struct{}{}
			return nil
		}),
	}, consumer.WithPollInterval(10*time.Millisecond))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx) }()
	e.write(t, noteWritten{Text: "a"})
	<-handled

	second := newPgConsumer(t, e.runtime(t), consumer.Config{Name: "shared"})
	if err := second.Drain(t.Context()); !errors.Is(err, consumer.ErrRunning) {
		t.Errorf("Drain on a second Runtime = %v, want ErrRunning", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if err := second.Drain(t.Context()); err != nil {
		t.Errorf("Drain after the first Runtime stopped = %v, want nil", err)
	}
}

func TestPgStoppedSurvivesRestart(t *testing.T) {
	e := newPgEnv(t, pgEnvConfig{})
	last := e.write(t, noteWritten{Text: "a"})
	broken := newPgConsumer(t, e.runtime(t), consumer.Config{
		Name: "restart",
		Handler: consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error {
			return consumer.Permanent(errors.New("broken"))
		}),
	})
	if err := broken.Drain(t.Context()); !errors.Is(err, consumer.ErrStopped) {
		t.Fatalf("Drain = %v, want ErrStopped", err)
	}

	rt := e.runtime(t)
	restarted := newPgConsumer(t, rt, consumer.Config{Name: "restart"})
	st, err := restarted.Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Stopped == nil || !strings.Contains(st.Stopped.Cause, "broken") || st.Pending != 1 {
		t.Errorf("Status after restart = %+v, want Stopped by broken with 1 pending", st)
	}
	if err := restarted.Drain(t.Context()); !errors.Is(err, consumer.ErrStopped) {
		t.Errorf("Drain after restart = %v, want ErrStopped", err)
	}
	if err := rt.Resume(t.Context(), "restart"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := restarted.Drain(t.Context()); err != nil {
		t.Fatalf("Drain after Resume: %v", err)
	}
	if st, err := restarted.Status(t.Context()); err != nil || st != (consumer.Status{Cursor: last}) {
		t.Errorf("Status after Resume = %+v, %v, want cursor %v only", st, err, last)
	}
}

func TestPgResumeOfUnattachedConsumer(t *testing.T) {
	e := newPgEnv(t, pgEnvConfig{})
	e.write(t, noteWritten{Text: "a"})
	broken := newPgConsumer(t, e.runtime(t), consumer.Config{
		Name: "operated",
		Handler: consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error {
			return consumer.Permanent(errors.New("broken"))
		}),
	})
	if err := broken.Drain(t.Context()); !errors.Is(err, consumer.ErrStopped) {
		t.Fatalf("Drain = %v, want ErrStopped", err)
	}

	operator := e.runtime(t)
	if err := operator.Resume(t.Context(), "operated"); err != nil {
		t.Fatalf("Resume from another Runtime: %v", err)
	}
	if err := operator.Resume(t.Context(), "unknown"); !errors.Is(err, consumer.ErrNotFound) {
		t.Errorf("Resume unknown = %v, want ErrNotFound", err)
	}
	if st, err := broken.Status(t.Context()); err != nil || st.Stopped != nil {
		t.Errorf("Status = %+v, %v, want not Stopped", st, err)
	}
}

// TestPgRunWakesOnNotify uses a poll interval far beyond the test timeout, so
// only the NOTIFY of the events trigger can deliver the event.
func TestPgRunWakesOnNotify(t *testing.T) {
	e := newPgEnv(t, pgEnvConfig{schema: "alt"})
	rt, listener := e.runtimeWithListener(t)
	waitListening(t, e.pool, listener, "alt.events")

	handled := make(chan string, 1)
	c := newPgConsumer(t, rt, consumer.Config{
		Name: "notified",
		Handler: consumer.HandlerFunc(func(_ context.Context, env eventstore.Envelope) error {
			handled <- env.EventID.String()
			return nil
		}),
	}, consumer.WithPollInterval(time.Hour))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// The first poll after acquiring the lease finds nothing; wait for it so
	// the event below can only arrive through NOTIFY.
	waitCursorRow(t, e.pool, "alt", "notified")
	e.write(t, noteWritten{Text: "a"})
	select {
	case <-handled:
	case <-time.After(10 * time.Second):
		t.Fatal("event not delivered after NOTIFY")
	}
}

// waitListening blocks until listener receives notifications on channel.
func waitListening(t *testing.T, pool *pgxpool.Pool, listener *notify.Listener, channel string) {
	t.Helper()
	sub, err := listener.Subscribe(channel)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(10 * time.Second)
	for {
		if _, err := pool.Exec(t.Context(), "SELECT pg_notify($1, 'probe')", channel); err != nil {
			t.Fatalf("notify: %v", err)
		}
		select {
		case <-sub.C():
			return
		case <-tick.C:
		case <-timeout:
			t.Fatal("listener never received on " + channel)
		}
	}
}

// waitCursorRow blocks until the Consumer has claimed its Cursor row.
func waitCursorRow(t *testing.T, pool *pgxpool.Pool, schema, name string) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(10 * time.Second)
	for {
		var exists bool
		err := pool.QueryRow(t.Context(),
			"SELECT EXISTS (SELECT 1 FROM "+schema+".consumer_cursors WHERE name = $1)", name).Scan(&exists)
		if err != nil {
			t.Fatalf("query cursor row: %v", err)
		}
		if exists {
			return
		}
		select {
		case <-tick.C:
		case <-timeout:
			t.Fatal("cursor row never created")
		}
	}
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
