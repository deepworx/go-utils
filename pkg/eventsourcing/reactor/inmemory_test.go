package reactor_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/reactortest"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/reactor"
)

func TestInMemoryContract(t *testing.T) {
	t.Parallel()
	reactortest.Run(t, func(_ *testing.T, codec *eventstore.Codec) reactortest.Backend {
		store := eventstore.NewInMemoryStore(codec)
		return reactortest.Backend{
			Store:      store,
			UnitOfWork: eventstore.NewInMemoryUnitOfWork(),
			Runtime:    consumer.NewInMemoryRuntime(consumer.InMemoryRuntimeConfig{Store: store}),
			Redeliver:  reactor.Redeliver,
		}
	}, reactortest.InBubble())
}

// A running Reactor redrives a Parked Stream on its own once the first
// step of the schedule has passed.
func TestInMemoryRunRedrivesOnSchedule(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		codec := eventstore.NewCodec()
		eventstore.Register[placed](codec)
		store := eventstore.NewInMemoryStore(codec)
		orders := eventstore.NewRepository[orderID, order](eventstore.RepositoryConfig{
			Store: store, UnitOfWork: eventstore.NewInMemoryUnitOfWork(),
		})

		var (
			fixed   atomic.Bool
			reacted atomic.Int32
		)
		r, err := reactor.New(consumer.NewInMemoryRuntime(consumer.InMemoryRuntimeConfig{Store: store}), reactor.Config{
			Name:    "run",
			StartAt: reactor.FromBeginning,
			Handler: consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error {
				if !fixed.Load() {
					return consumer.Permanent(errors.New("down"))
				}
				reacted.Add(1)
				return nil
			}),
		}, reactor.WithLogger(slog.New(slog.DiscardHandler)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Go(func() { _ = consumer.Group{r.Consumer()}.Run(ctx) })
		defer wg.Wait()
		defer cancel()

		writer := ctxutil.WithClaims(ctxutil.WithTenantID(context.Background(), "t1"), ctxutil.Claims{UserID: "user-1"})
		_, err = orders.Create(writer, orderID(uuid.Must(uuid.NewV7())), func(_ context.Context, o *order) error {
			aggregate.Raise(o, placed{Note: "x"})
			return nil
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		synctest.Wait()
		if st := status(t, r); st.ParkedStreams != 1 {
			t.Fatalf("Status = %+v, want one Parked Stream", st)
		}
		fixed.Store(true)

		time.Sleep(45 * time.Second)
		synctest.Wait()
		if got := reacted.Load(); got != 0 {
			t.Fatalf("reactions before the first step = %d, want 0", got)
		}

		time.Sleep(15 * time.Second)
		synctest.Wait()
		if got := reacted.Load(); got != 1 {
			t.Errorf("reactions after the first step = %d, want 1", got)
		}
		if st := status(t, r); st.ParkedStreams != 0 || st.ParkedEvents != 0 {
			t.Errorf("Status = %+v, want nothing parked", st)
		}
	})
}

func status(t *testing.T, r *reactor.Reactor) reactor.Status {
	t.Helper()
	st, err := r.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return st
}
