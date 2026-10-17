package reactor_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/internal/reactortest"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/reactor"
	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

// The Pg tests do not run in parallel: their databases share one cluster,
// and pg_snapshot_xmin is cluster-wide, so a transaction in one test would
// hide fresh events from Drain in another.

func TestPgContract(t *testing.T) {
	reactortest.Run(t, func(t *testing.T, codec *eventstore.Codec) reactortest.Backend {
		pool := pgtest.New(t)
		store, err := eventstore.NewPgStore(eventstore.PgStoreConfig{Pool: pool, Codec: codec})
		if err != nil {
			t.Fatalf("NewPgStore: %v", err)
		}
		uow, err := eventstore.NewPgUnitOfWork(eventstore.PgUnitOfWorkConfig{UnitOfWork: postgres.NewUnitOfWork(pool)})
		if err != nil {
			t.Fatalf("NewPgUnitOfWork: %v", err)
		}
		return reactortest.Backend{
			Store:      store,
			UnitOfWork: uow,
			Runtime:    newPgRuntime(t, pool, store),
			Redeliver:  reactor.Redeliver,
			Discarded:  pgDiscarded(pool),
		}
	}, reactortest.Sequential())
}

func newPgRuntime(t *testing.T, pool *pgxpool.Pool, store *eventstore.PgStore) *consumer.PgRuntime {
	t.Helper()
	claims := lease.NewClaims(pool, lease.Config{PingInterval: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = claims.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	rt, err := consumer.NewPgRuntime(consumer.PgRuntimeConfig{Pool: pool, Store: store, Claims: claims})
	if err != nil {
		t.Fatalf("NewPgRuntime: %v", err)
	}
	return rt
}

// pgDiscarded reads the audit rows Discard keeps.
func pgDiscarded(pool *pgxpool.Pool) func(ctx context.Context, reactorName string) ([]reactortest.Discarded, error) {
	return func(ctx context.Context, reactorName string) ([]reactortest.Discarded, error) {
		rows, err := pool.Query(ctx, `SELECT event_id, discarded_by FROM parked_events
			WHERE consumer = $1 AND discarded_at IS NOT NULL ORDER BY stream_position`, reactorName)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []reactortest.Discarded
		for rows.Next() {
			var d reactortest.Discarded
			if err := rows.Scan(&d.EventID, &d.By); err != nil {
				return nil, err
			}
			out = append(out, d)
		}
		return out, rows.Err()
	}
}

// A park list that cannot be written makes the Reactor retry and then go
// Stopped, because moving the Cursor past an unrecorded event would lose it.
func TestPgParkWriteFailureStops(t *testing.T) {
	pool := pgtest.New(t)
	codec := eventstore.NewCodec()
	eventstore.Register[placed](codec)
	store, err := eventstore.NewPgStore(eventstore.PgStoreConfig{Pool: pool, Codec: codec})
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	uow, err := eventstore.NewPgUnitOfWork(eventstore.PgUnitOfWorkConfig{UnitOfWork: postgres.NewUnitOfWork(pool)})
	if err != nil {
		t.Fatalf("NewPgUnitOfWork: %v", err)
	}
	orders := eventstore.NewRepository[orderID, order](eventstore.RepositoryConfig{Store: store, UnitOfWork: uow})
	ctx := ctxutil.WithClaims(ctxutil.WithTenantID(context.Background(), "t1"), ctxutil.Claims{UserID: "user-1"})
	_, err = orders.Create(ctx, orderID(uuid.Must(uuid.NewV7())), func(_ context.Context, o *order) error {
		aggregate.Raise(o, placed{Note: "x"})
		return nil
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE parked_events ADD CONSTRAINT no_parking CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("block parking: %v", err)
	}

	var calls atomic.Int32
	r, err := reactor.New(newPgRuntime(t, pool, store), reactor.Config{
		Name:    "unwritable",
		StartAt: reactor.FromBeginning,
		Handler: consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error {
			calls.Add(1)
			return consumer.Permanent(errors.New("hopeless"))
		}),
	}, reactor.WithRetry(consumer.Retry{MaxAttempts: 2, Base: time.Millisecond, Max: time.Millisecond}),
		reactor.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := r.Consumer().Drain(context.Background()); !errors.Is(err, consumer.ErrStopped) {
		t.Fatalf("Drain = %v, want ErrStopped", err)
	}
	st, err := r.Consumer().Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Cursor.IsZero() || st.Stopped == nil {
		t.Errorf("Status = %+v, want Stopped at the zero Cursor", st)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("reactions = %d, want one per park attempt", got)
	}
}

type orderID uuid.UUID

type placed struct {
	Note string `json:"note"`
}

func (placed) EventType() string { return "Test.Order.Placed" }

type order struct {
	aggregate.Base[orderID]
}

func (o *order) StreamType() string { return "Test.Order" }

func (o *order) Apply(aggregate.Event) {}
