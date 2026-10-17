package projection_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/projection"
	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

// The Pg tests do not run in parallel: pg_snapshot_xmin is cluster-wide, so
// a transaction in one test's database would hide fresh events from Drain
// in another.

func TestPgContract(t *testing.T) {
	runContract(t, func(t *testing.T, codec *eventstore.Codec) backend {
		return newPgEnv(t, codec).backend
	}, false)
}

type pgEnv struct {
	backend
	pool *pgxpool.Pool
}

func newPgEnv(t *testing.T, codec *eventstore.Codec) pgEnv {
	t.Helper()
	pool := pgtest.New(t)
	store, err := eventstore.NewPgStore(eventstore.PgStoreConfig{Pool: pool, Codec: codec, Schema: "public"})
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	uow, err := eventstore.NewPgUnitOfWork(eventstore.PgUnitOfWorkConfig{
		UnitOfWork: postgres.NewUnitOfWork(pool), Schema: "public",
	})
	if err != nil {
		t.Fatalf("NewPgUnitOfWork: %v", err)
	}

	claims := lease.NewClaims(pool, lease.Config{PingInterval: 50 * time.Millisecond})
	listener := notify.NewListener(pool, notify.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = claims.Run(ctx) })
	wg.Go(func() { _ = listener.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	rt, err := consumer.NewPgRuntime(consumer.PgRuntimeConfig{Pool: pool, Store: store, Listener: listener, Claims: claims})
	if err != nil {
		t.Fatalf("NewPgRuntime: %v", err)
	}

	model := newPgModel(t, pool)
	return pgEnv{
		pool: pool,
		backend: backend{
			store:     store,
			uow:       uow,
			runtime:   rt,
			model:     model,
			subscribe: func(t *testing.T) *projection.Subscription { return pgSubscribe(t, rt, pool) },
		},
	}
}

// pgSubscribe subscribes and waits until the LISTEN took effect, by sending
// probe changes until one arrives.
func pgSubscribe(t *testing.T, rt consumer.Runtime, pool *pgxpool.Pool) *projection.Subscription {
	t.Helper()
	sub, err := projection.Subscribe(rt)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	probe := `{"tenant":"","entity":"probe","id":"` + uuid.Nil.String() + `"}`
	deadline := time.Now().Add(receiveTimeout)
	for time.Now().Before(deadline) {
		if _, err := pool.Exec(t.Context(), `SELECT pg_notify('public.changes', $1)`, probe); err != nil {
			t.Fatalf("probe: %v", err)
		}
		select {
		case <-sub.C():
			drainProbes(sub)
			return sub
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("subscription never started listening")
	return nil
}

// drainProbes drops probe changes still in flight.
func drainProbes(sub *projection.Subscription) {
	for {
		select {
		case <-sub.C():
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

// pgModel keeps version N in table notes_vN.
type pgModel struct {
	pool *pgxpool.Pool
}

func newPgModel(t *testing.T, pool *pgxpool.Pool) *pgModel {
	t.Helper()
	for _, v := range []int{1, 2} {
		_, err := pool.Exec(t.Context(), `CREATE TABLE `+table(v)+` (
			seq  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			text TEXT NOT NULL)`)
		if err != nil {
			t.Fatalf("create %s: %v", table(v), err)
		}
	}
	return &pgModel{pool: pool}
}

func table(version int) string {
	return pgx.Identifier{"notes_v" + strconv.Itoa(version)}.Sanitize()
}

func (m *pgModel) Add(ctx context.Context, version int, text string) error {
	_, err := postgres.Conn(ctx, m.pool).Exec(ctx, `INSERT INTO `+table(version)+` (text) VALUES ($1)`, text)
	return err
}

func (m *pgModel) Texts(ctx context.Context, version int) ([]string, error) {
	rows, err := m.pool.Query(ctx, `SELECT text FROM `+table(version)+` ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (m *pgModel) Clear(ctx context.Context, version int) error {
	_, err := postgres.Conn(ctx, m.pool).Exec(ctx, `DELETE FROM `+table(version))
	return err
}

// switchToV2 is release N+1's migration of the documented version bump: a
// guard that fails while notes@v2 lags, then the view switch.
const switchToV2 = `
DO $$
BEGIN
    IF public.consumer_pending('notes@v2', 0) > 0 THEN
        RAISE EXCEPTION 'projection notes@v2 is not caught up';
    END IF;
END
$$;
CREATE OR REPLACE VIEW notes AS SELECT seq, text FROM notes_v2;`

func TestPgGuardSwitchesViewOnceShadowCaughtUp(t *testing.T) {
	codec := newCodec()
	e := newPgEnv(t, codec)
	h := harness{backend: e.backend, codec: codec, notes: eventstore.NewRepository[noteID, note](
		eventstore.RepositoryConfig{Store: e.store, UnitOfWork: e.uow})}

	// Release N-1: readers query the view over the active version.
	if _, err := e.pool.Exec(t.Context(), `CREATE VIEW notes AS SELECT seq, text FROM notes_v1`); err != nil {
		t.Fatalf("create view: %v", err)
	}
	h.write(t, "a")
	h.write(t, "b")

	// Release N: notes_v2 exists and builds as the shadow.
	p := h.newProjection(t, 1, 2)
	active, shadow := p.Consumers()[0], p.Consumers()[1]
	drain(t, active)

	if err := migrate(t, e.pool, switchToV2); err == nil {
		t.Fatal("switch migration succeeded while the shadow lags, want the guard to raise")
	}
	if got := viewTexts(t, e.pool); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("view = %v after the failed switch, want v1's [a b]", got)
	}

	drain(t, shadow)
	if !caughtUp(t, p) {
		t.Fatal("CaughtUp = false after the shadow drained")
	}
	if err := migrate(t, e.pool, switchToV2); err != nil {
		t.Fatalf("switch migration once caught up: %v", err)
	}

	// The view now reads v2: a note only v2 has shows.
	h.write(t, "c")
	drain(t, shadow)
	if got := viewTexts(t, e.pool); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("view = %v after the switch, want v2's [a b c]", got)
	}
}

// migrate runs sql in one transaction, as golang-migrate does.
func migrate(t *testing.T, pool *pgxpool.Pool, sql string) error {
	t.Helper()
	return pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), sql); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		return nil
	})
}

func viewTexts(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT text FROM notes ORDER BY seq`)
	if err != nil {
		t.Fatalf("read view: %v", err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read view: %v", err)
	}
	return texts
}

func TestPgSubscribeWithoutListener(t *testing.T) {
	codec := newCodec()
	pool := pgtest.New(t)
	store, err := eventstore.NewPgStore(eventstore.PgStoreConfig{Pool: pool, Codec: codec, Schema: "public"})
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	claims := lease.NewClaims(pool, lease.Config{})
	rt, err := consumer.NewPgRuntime(consumer.PgRuntimeConfig{Pool: pool, Store: store, Claims: claims})
	if err != nil {
		t.Fatalf("NewPgRuntime: %v", err)
	}
	if _, err := projection.Subscribe(rt); err == nil {
		t.Error("Subscribe without a Listener = nil, want an error")
	}
}
