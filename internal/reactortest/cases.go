package reactortest

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/reactor"
)

type testCase struct {
	name string
	fn   func(t *testing.T)
}

func cases(s suite) []testCase {
	return []testCase{
		{"StartAtRequired", s.startAtRequired},
		{"FromNowSkipsHistory", s.fromNowSkipsHistory},
		{"FromBeginningReactsToHistory", s.fromBeginningReactsToHistory},
		{"CommandMetadata", s.commandMetadata},
		{"ParksFailingStreamWhileOthersProgress", s.parksFailingStream},
		{"RetriesThenParks", s.retriesThenParks},
		{"ParksImmediately/Permanent", s.parksImmediately(func() error { return errHopeless })},
		{"ParksImmediately/Panic", s.parksImmediately(func() error { panic("kaboom") })},
		{"ParksImmediately/DecodeError", s.parksUndecodable},
		{"TimeoutFailsReaction", s.timeoutFailsReaction},
		{"BlockedTenantParks", s.blockedTenantParks},
		{"RedriveReplaysInOrder", s.redriveReplaysInOrder},
		{"RedriveStopsAtFirstFailure", s.redriveStopsAtFirstFailure},
		{"Discard", s.discard},
		{"ParkTwiceIsNoop", s.parkTwiceIsNoop},
		{"AutoRedriveReleasesAtFirstStep", s.autoRedriveReleases},
		{"AutoRedriveExhausts", s.autoRedriveExhausts},
		{"NoRedriveSteps", s.noRedriveSteps},
		{"StatusCountsParked", s.statusCountsParked},
		{"ConcurrentRedrives", s.concurrentRedrives},
		{"RedriveDoesNotBlockLiveHold", s.redriveDoesNotBlockLiveHold},
		{"DiscardDuringRedrive", s.discardDuringRedrive},
	}
}

type suite struct {
	newBackend func(t *testing.T, codec *eventstore.Codec) Backend
}

type harness struct {
	t         *testing.T
	backend   Backend
	orders    *eventstore.Repository[orderID, order, *order]
	shipments *eventstore.Repository[shipmentID, shipment, *shipment]
	clock     *clock
}

func (s suite) setup(t *testing.T) harness {
	t.Helper()
	b := s.newBackend(t, newCodec())
	cfg := eventstore.RepositoryConfig{Store: b.Store, UnitOfWork: b.UnitOfWork}
	return harness{
		t:         t,
		backend:   b,
		orders:    eventstore.NewRepository[orderID, order](cfg),
		shipments: eventstore.NewRepository[shipmentID, shipment](cfg),
		clock:     &clock{},
	}
}

func userCtx(tenant string) context.Context {
	ctx := ctxutil.WithTenantID(context.Background(), tenant)
	return ctxutil.WithClaims(ctx, ctxutil.Claims{UserID: "user-1"})
}

// place writes a new order Stream with one placed event per note.
func (h harness) place(ctx context.Context, notes ...string) orderID {
	h.t.Helper()
	id := orderID(uuid.Must(uuid.NewV7()))
	_, err := h.orders.Create(ctx, id, func(_ context.Context, o *order) error {
		for _, n := range notes {
			aggregate.Raise(o, placed{Note: n})
		}
		return nil
	})
	if err != nil {
		h.t.Fatalf("place: %v", err)
	}
	return id
}

// append writes events to an existing order Stream.
func (h harness) append(ctx context.Context, id orderID, events ...aggregate.Event) {
	h.t.Helper()
	_, err := h.orders.Update(ctx, id, func(_ context.Context, o *order) error {
		for _, e := range events {
			aggregate.Raise(o, e)
		}
		return nil
	})
	if err != nil {
		h.t.Fatalf("append: %v", err)
	}
}

func (h harness) newReactor(cfg reactor.Config, opts ...reactor.Option) *reactor.Reactor {
	h.t.Helper()
	if cfg.StartAt == 0 {
		cfg.StartAt = reactor.FromBeginning
	}
	opts = append([]reactor.Option{fastRetry, quiet, reactor.WithClock(h.clock.now)}, opts...)
	r, err := reactor.New(h.backend.Runtime, cfg, opts...)
	if err != nil {
		h.t.Fatalf("reactor.New: %v", err)
	}
	return r
}

var (
	quiet     = reactor.WithLogger(slog.New(slog.DiscardHandler))
	fastRetry = reactor.WithRetry(consumer.Retry{MaxAttempts: 3, Base: time.Millisecond, Max: 2 * time.Millisecond})
)

func (h harness) drain(r *reactor.Reactor) {
	h.t.Helper()
	if err := r.Drain(context.Background()); err != nil {
		h.t.Fatalf("Drain: %v", err)
	}
}

func (h harness) parked(r *reactor.Reactor, tenant string) []reactor.Parked {
	h.t.Helper()
	p, err := r.Parked(context.Background(), tenant)
	if err != nil {
		h.t.Fatalf("Parked: %v", err)
	}
	return p
}

// reactions records the notes a Reactor reacted to, in order.
type reactions struct {
	mu    sync.Mutex
	notes []string
}

func (r *reactions) add(note string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, note)
}

func (r *reactions) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.notes)
}

// script returns a handler that runs react for every placed event and
// records the note when react succeeds.
func script(rec *reactions, react func(ctx context.Context, note string) error) consumer.Handler {
	return consumer.HandlerFunc(func(ctx context.Context, env eventstore.Envelope) error {
		p, ok := env.Event.(placed)
		if !ok {
			return nil
		}
		if err := react(ctx, p.Note); err != nil {
			return err
		}
		rec.add(p.Note)
		return nil
	})
}

func succeed(context.Context, string) error { return nil }

func (s suite) startAtRequired(t *testing.T) {
	h := s.setup(t)
	_, err := reactor.New(h.backend.Runtime, reactor.Config{
		Name:    "no_start",
		Handler: script(&reactions{}, succeed),
	})
	if !errors.Is(err, reactor.ErrStartAtRequired) {
		t.Fatalf("New = %v, want ErrStartAtRequired", err)
	}
}

func (s suite) fromNowSkipsHistory(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.place(ctx, "history")

	var rec reactions
	r := h.newReactor(reactor.Config{Name: "from_now", Handler: script(&rec, succeed), StartAt: reactor.FromNow})
	h.drain(r)
	h.place(ctx, "new")
	h.drain(r)

	if got := rec.get(); !slices.Equal(got, []string{"new"}) {
		t.Fatalf("reacted to %v, want only [new]", got)
	}
}

func (s suite) fromBeginningReactsToHistory(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.place(ctx, "history")

	var rec reactions
	r := h.newReactor(reactor.Config{Name: "from_beginning", Handler: script(&rec, succeed), StartAt: reactor.FromBeginning})
	h.drain(r)
	h.place(ctx, "new")
	h.drain(r)

	if got := rec.get(); !slices.Equal(got, []string{"history", "new"}) {
		t.Fatalf("reacted to %v, want [history new]", got)
	}
}
