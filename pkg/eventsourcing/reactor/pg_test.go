package reactor_test

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

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
func pgDiscarded(pool *pgxpool.Pool) func(ctx context.Context, r *reactor.Reactor) ([]reactortest.Discarded, error) {
	return func(ctx context.Context, r *reactor.Reactor) ([]reactortest.Discarded, error) {
		rows, err := pool.Query(ctx, `SELECT event_id, discarded_by FROM parked_events
			WHERE consumer = $1 AND discarded_at IS NOT NULL ORDER BY discarded_at, stream_position`, r.Consumer().Name())
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

// A park list that cannot be written makes the Reactor retry the write and
// then go Stopped, because moving the Cursor past an unrecorded event would
// lose it. The reaction itself runs only once: a retry repeats the write,
// not the outbound side effects.
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
	if got := calls.Load(); got != 1 {
		t.Errorf("reactions = %d, want 1 across all park attempts", got)
	}
}

// Two Runtimes play two pods running the same Reactor. Only the lease
// owner reports the park list gauges.
func TestPgParkedGaugesOnlyFromLeaseOwner(t *testing.T) {
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
	writer := ctxutil.WithClaims(ctxutil.WithTenantID(context.Background(), "t1"), ctxutil.Claims{UserID: "user-1"})
	_, err = orders.Create(writer, orderID(uuid.Must(uuid.NewV7())), func(_ context.Context, o *order) error {
		aggregate.Raise(o, placed{Note: "a"})
		aggregate.Raise(o, placed{Note: "b"})
		return nil
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	var (
		readers  []*sdkmetric.ManualReader
		reactors []*reactor.Reactor
	)
	for range 2 {
		reader := sdkmetric.NewManualReader()
		r, err := reactor.New(newPgRuntime(t, pool, store), reactor.Config{
			Name:    "owner",
			StartAt: reactor.FromBeginning,
			Handler: consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error {
				return consumer.Permanent(errors.New("down"))
			}),
		}, reactor.WithLogger(slog.New(slog.DiscardHandler)),
			reactor.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		readers = append(readers, reader)
		reactors = append(reactors, r)
		wg.Go(func() { _ = r.Consumer().Run(ctx) })
	}
	waitParked(t, reactors[0], 2)

	want := attribute.NewSet(attribute.String("eventsourcing.consumer", "owner"))
	wantGauges := map[string]int64{
		"eventsourcing.reactor.parked.streams":   1,
		"eventsourcing.reactor.parked.events":    2,
		"eventsourcing.reactor.parked.exhausted": 0,
	}
	owners := 0
	for i, reader := range readers {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("pod %d: collect: %v", i, err)
		}
		got := make(map[string]int64)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if _, ok := wantGauges[m.Name]; !ok {
					continue
				}
				g, ok := m.Data.(metricdata.Gauge[int64])
				if !ok || len(g.DataPoints) == 0 {
					continue
				}
				if len(g.DataPoints) != 1 || !g.DataPoints[0].Attributes.Equals(&want) {
					t.Errorf("pod %d: %s = %+v, want one point labelled %v", i, m.Name, g.DataPoints, want)
				}
				got[m.Name] = g.DataPoints[0].Value
			}
		}
		if len(got) == 0 {
			continue
		}
		owners++
		if !maps.Equal(got, wantGauges) {
			t.Errorf("pod %d: gauges = %v, want %v", i, got, wantGauges)
		}
	}
	if owners != 1 {
		t.Errorf("%d pods report the park list gauges, want exactly 1", owners)
	}
}

// waitParked polls r's Status until events are parked or the test times
// out.
func waitParked(t *testing.T, r *reactor.Reactor, events int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := r.Status(context.Background())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.ParkedEvents == events && st.Pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Status = %+v, want %d parked events", st, events)
		}
		time.Sleep(10 * time.Millisecond)
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
