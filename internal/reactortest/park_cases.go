package reactortest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/reactor"
)

var errHopeless = consumer.Permanent(errors.New("hopeless"))

// history returns the envelopes of an order Stream of tenant.
func (h harness) history(tenant string, id orderID) []eventstore.Envelope {
	h.t.Helper()
	envs, err := h.orders.History(userCtx(tenant), id)
	if err != nil {
		h.t.Fatalf("History: %v", err)
	}
	return envs
}

func streamOf(tenant string, id orderID) reactor.Stream {
	return reactor.Stream{Tenant: tenant, ID: uuid.UUID(id)}
}

// failOn fails the notes in fail with err and succeeds otherwise.
func failOn(err error, fail ...string) func(context.Context, string) error {
	return func(_ context.Context, note string) error {
		if slices.Contains(fail, note) {
			return err
		}
		return nil
	}
}

// notesOf returns the notes of parked events, in order; "" for an event
// that is not placed.
func (h harness) notesOf(parked []reactor.Parked) []string {
	h.t.Helper()
	notes := make([]string, 0, len(parked))
	for _, p := range parked {
		for _, env := range h.history(p.Stream.Tenant, orderID(p.Stream.ID)) {
			if env.EventID != p.EventID {
				continue
			}
			pl, _ := env.Event.(placed)
			notes = append(notes, pl.Note)
		}
	}
	return notes
}

func (h harness) assertCaughtUp(r *reactor.Reactor) {
	h.t.Helper()
	st, err := r.Consumer().Status(context.Background())
	if err != nil {
		h.t.Fatalf("Status: %v", err)
	}
	if st.Pending != 0 || st.Stopped != nil {
		h.t.Fatalf("Status = %+v, want nothing pending and not Stopped", st)
	}
}

// commandMetadata checks the metadata of a command issued by a reaction,
// on a Redrive under an operator's ctx and on the live path.
func (s suite) commandMetadata(t *testing.T) {
	h := s.setup(t)
	id := h.place(ctxutil.WithCorrelationID(userCtx("t1"), "corr-1"), "ship")
	cause := h.history("t1", id)[0].EventID

	var (
		failed    atomic.Bool
		mu        sync.Mutex
		shipments []shipmentID
	)
	r := h.newReactor(reactor.Config{
		Name: "ship",
		Handler: script(&reactions{}, func(ctx context.Context, note string) error {
			if failed.CompareAndSwap(false, true) {
				return errHopeless
			}
			sid := shipmentID(uuid.Must(uuid.NewV7()))
			mu.Lock()
			shipments = append(shipments, sid)
			mu.Unlock()
			_, err := h.shipments.Create(ctx, sid, func(_ context.Context, s *shipment) error {
				aggregate.Raise(s, requested{Order: note})
				return nil
			})
			return err
		}),
	})
	h.drain(r)

	operator := ctxutil.WithRequestID(userCtx("ops"), "req-op")
	if _, err := r.Redrive(operator, streamOf("t1", id)); err != nil {
		t.Fatalf("Redrive: %v", err)
	}
	live := h.place(ctxutil.WithCorrelationID(userCtx("t1"), "corr-2"), "ship")
	h.drain(r)

	tests := []struct {
		name        string
		causation   uuid.UUID
		correlation string
	}{
		{"Redrive", cause, "corr-1"},
		{"Live", h.history("t1", live)[0].EventID, "corr-2"},
	}
	if len(shipments) != len(tests) {
		t.Fatalf("shipments = %d, want %d", len(shipments), len(tests))
	}
	for i, tt := range tests {
		envs, err := h.shipments.History(userCtx("t1"), shipments[i])
		if err != nil {
			t.Fatalf("%s: shipment History: %v", tt.name, err)
		}
		got := envs[0]
		if got.Actor != "system:reactor:ship" || got.CausationID != tt.causation || got.CorrelationID != tt.correlation {
			t.Errorf("%s: command metadata = actor %q causation %s correlation %q, want system:reactor:ship %s %s",
				tt.name, got.Actor, got.CausationID, got.CorrelationID, tt.causation, tt.correlation)
		}
	}
}

func (s suite) parksFailingStream(t *testing.T) {
	h := s.setup(t)
	bad := h.place(userCtx("t1"), "fail", "after")
	h.place(userCtx("t1"), "ok1")
	h.place(userCtx("t2"), "ok2")

	var rec reactions
	r := h.newReactor(reactor.Config{Name: "parks", Handler: script(&rec, failOn(errHopeless, "fail"))})
	h.drain(r)

	if got := rec.get(); !slices.Equal(got, []string{"ok1", "ok2"}) {
		t.Fatalf("reacted to %v, want [ok1 ok2]", got)
	}
	h.assertCaughtUp(r)

	parked := h.parked(r, "t1")
	if got := h.notesOf(parked); !slices.Equal(got, []string{"fail", "after"}) {
		t.Fatalf("parked %v, want [fail after]", got)
	}
	head, behind := parked[0], parked[1]
	if head.Stream != streamOf("t1", bad) || head.Cause == "" || head.Attempts != 1 || head.StreamPosition != 1 ||
		head.EventType != "Test.Order.Placed" || head.StreamType != "Test.Order" || head.ParkedAt.IsZero() {
		t.Errorf("head = %+v, want Stream t1/%s, a cause, 1 attempt, position 1", head, uuid.UUID(bad))
	}
	if behind.Cause != "" || behind.Attempts != 0 || behind.StreamPosition != 2 {
		t.Errorf("held = %+v, want no cause, 0 attempts, position 2", behind)
	}
	if got := h.parked(r, "t2"); len(got) != 0 {
		t.Errorf("Parked(t2) = %v, want none", got)
	}
	if got := h.parked(r, ""); len(got) != 2 {
		t.Errorf("Parked(all) = %d events, want 2", len(got))
	}

	h.append(userCtx("t1"), bad, placed{Note: "later"})
	h.place(userCtx("t1"), "ok3")
	h.drain(r)
	if got := rec.get(); !slices.Equal(got, []string{"ok1", "ok2", "ok3"}) {
		t.Errorf("reacted to %v, want [ok1 ok2 ok3]", got)
	}
	if got := h.notesOf(h.parked(r, "t1")); !slices.Equal(got, []string{"fail", "after", "later"}) {
		t.Errorf("parked %v, want [fail after later]", got)
	}
}

func (s suite) retriesThenParks(t *testing.T) {
	h := s.setup(t)
	h.place(userCtx("t1"), "flaky")

	var calls atomic.Int32
	r := h.newReactor(reactor.Config{Name: "retries", Handler: script(&reactions{}, func(context.Context, string) error {
		calls.Add(1)
		return errors.New("503")
	})})
	h.drain(r)

	if got := calls.Load(); got != 3 {
		t.Errorf("reactions = %d, want 3", got)
	}
	parked := h.parked(r, "t1")
	if len(parked) != 1 || parked[0].Attempts != 3 || !strings.Contains(parked[0].Cause, "503") {
		t.Fatalf("parked = %+v, want one event after 3 attempts caused by 503", parked)
	}
	h.assertCaughtUp(r)
}

func (s suite) parksImmediately(fail func() error) func(t *testing.T) {
	return func(t *testing.T) {
		h := s.setup(t)
		h.place(userCtx("t1"), "x")

		var calls atomic.Int32
		r := h.newReactor(reactor.Config{Name: "immediate", Handler: script(&reactions{}, func(context.Context, string) error {
			calls.Add(1)
			return fail()
		})})
		h.drain(r)

		if got := calls.Load(); got != 1 {
			t.Errorf("reactions = %d, want 1", got)
		}
		if parked := h.parked(r, "t1"); len(parked) != 1 || parked[0].Attempts != 1 || parked[0].Cause == "" {
			t.Fatalf("parked = %+v, want one event after 1 attempt", parked)
		}
		h.assertCaughtUp(r)
	}
}

func (s suite) parksUndecodable(t *testing.T) {
	h := s.setup(t)
	id := h.place(userCtx("t1"), "first")
	h.append(userCtx("t1"), id, poisoned{Value: "x"}, placed{Note: "after"})

	var rec reactions
	r := h.newReactor(reactor.Config{Name: "undecodable", Handler: script(&rec, succeed)})
	h.drain(r)

	if got := rec.get(); !slices.Equal(got, []string{"first"}) {
		t.Errorf("reacted to %v, want [first]", got)
	}
	parked := h.parked(r, "t1")
	if len(parked) != 2 || parked[0].EventType != "Test.Order.Poisoned" || !strings.Contains(parked[0].Cause, "poisoned payload") ||
		parked[0].Attempts != 1 || parked[1].Cause != "" {
		t.Fatalf("parked = %+v, want the poisoned event as head and one held behind it", parked)
	}
	h.assertCaughtUp(r)
}

func (s suite) timeoutFailsReaction(t *testing.T) {
	h := s.setup(t)
	h.place(userCtx("t1"), "hang")

	var calls atomic.Int32
	r := h.newReactor(reactor.Config{Name: "timeout", Handler: script(&reactions{}, func(ctx context.Context, _ string) error {
		calls.Add(1)
		<-ctx.Done()
		return ctx.Err()
	})}, reactor.WithTimeout(20*time.Millisecond))
	h.drain(r)

	if got := calls.Load(); got != 3 {
		t.Errorf("reactions = %d, want 3", got)
	}
	if parked := h.parked(r, "t1"); len(parked) != 1 || !strings.Contains(parked[0].Cause, "deadline exceeded") {
		t.Fatalf("parked = %+v, want one event that timed out", parked)
	}
	h.assertCaughtUp(r)
}

// blockedTenantParks: the app refuses commands for a blocked Tenant
// (ADR-0009), which Parks the Stream and leaves other Tenants flowing.
func (s suite) blockedTenantParks(t *testing.T) {
	h := s.setup(t)
	h.place(userCtx("blocked"), "b")
	h.place(userCtx("t1"), "a")

	var rec reactions
	r := h.newReactor(reactor.Config{Name: "blocked", Handler: script(&rec, func(ctx context.Context, _ string) error {
		if tenant, _ := ctxutil.TenantID(ctx); tenant == "blocked" {
			return errors.New("tenant blocked")
		}
		return nil
	})})
	h.drain(r)

	if got := rec.get(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("reacted to %v, want [a]", got)
	}
	if parked := h.parked(r, "blocked"); len(parked) != 1 || !strings.Contains(parked[0].Cause, "tenant blocked") {
		t.Fatalf("parked = %+v, want the blocked Tenant's event", parked)
	}
	h.assertCaughtUp(r)
}

func (s suite) redriveReplaysInOrder(t *testing.T) {
	h := s.setup(t)
	id := h.place(userCtx("t1"), "fail", "a", "b")

	var broken atomic.Bool
	broken.Store(true)
	var rec reactions
	r := h.newReactor(reactor.Config{Name: "redrive", Handler: script(&rec, func(context.Context, string) error {
		if broken.Load() {
			return errHopeless
		}
		return nil
	})})
	h.drain(r)
	before, err := r.Consumer().Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	broken.Store(false)
	res, err := r.Redrive(userCtx("ops"), streamOf("t1", id))
	if err != nil {
		t.Fatalf("Redrive: %v", err)
	}
	if res != (reactor.RedriveResult{Replayed: 3, Remaining: 0}) {
		t.Errorf("Redrive = %+v, want 3 replayed, 0 remaining", res)
	}
	if got := rec.get(); !slices.Equal(got, []string{"fail", "a", "b"}) {
		t.Errorf("reacted to %v, want [fail a b]", got)
	}
	if got := h.parked(r, ""); len(got) != 0 {
		t.Errorf("parked after Redrive = %+v, want none", got)
	}
	after, err := r.Consumer().Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if after.Cursor != before.Cursor {
		t.Errorf("Cursor moved by Redrive: %v -> %v", before.Cursor, after.Cursor)
	}

	h.append(userCtx("t1"), id, placed{Note: "c"})
	h.drain(r)
	if got := rec.get(); !slices.Equal(got, []string{"fail", "a", "b", "c"}) {
		t.Errorf("reacted to %v, want the released Stream to flow live", got)
	}
	if _, err := r.Redrive(userCtx("ops"), streamOf("t1", id)); !errors.Is(err, reactor.ErrNotParked) {
		t.Errorf("Redrive of a released Stream = %v, want ErrNotParked", err)
	}
	if _, err := r.Redrive(userCtx("ops"), streamOf("t2", id)); !errors.Is(err, reactor.ErrNotParked) {
		t.Errorf("Redrive of another Tenant's Stream = %v, want ErrNotParked", err)
	}
}

func (s suite) redriveStopsAtFirstFailure(t *testing.T) {
	h := s.setup(t)
	id := h.place(userCtx("t1"), "fail", "a", "b")

	var broken sync.Map
	broken.Store("fail", true)
	broken.Store("a", true)
	var rec reactions
	r := h.newReactor(reactor.Config{Name: "redrive_stops", Handler: script(&rec, func(_ context.Context, note string) error {
		if _, ok := broken.Load(note); ok {
			return consumer.Permanent(errors.New(note + " broken"))
		}
		return nil
	})})
	h.drain(r)

	broken.Delete("fail")
	res, err := r.Redrive(userCtx("ops"), streamOf("t1", id))
	if err == nil || errors.Is(err, reactor.ErrNotParked) || !strings.Contains(err.Error(), "a broken") {
		t.Fatalf("Redrive = %v, want the failure of a", err)
	}
	if res != (reactor.RedriveResult{Replayed: 1, Remaining: 2}) {
		t.Errorf("Redrive = %+v, want 1 replayed, 2 remaining", res)
	}
	if got := rec.get(); !slices.Equal(got, []string{"fail"}) {
		t.Errorf("reacted to %v, want [fail]", got)
	}
	parked := h.parked(r, "t1")
	if got := h.notesOf(parked); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("parked %v, want [a b]", got)
	}
	if parked[0].Attempts != 1 || !strings.Contains(parked[0].Cause, "a broken") {
		t.Errorf("new head = %+v, want 1 attempt caused by a broken", parked[0])
	}
}

func (s suite) discard(t *testing.T) {
	h := s.setup(t)
	held := h.place(userCtx("t1"), "fail", "a")
	single := h.place(userCtx("t1"), "fail")

	var broken atomic.Bool
	broken.Store(true)
	var rec reactions
	r := h.newReactor(reactor.Config{Name: "discard", Handler: script(&rec, func(_ context.Context, note string) error {
		if note == "fail" && broken.Load() {
			return errHopeless
		}
		return nil
	})})
	h.drain(r)
	failed := h.history("t1", held)[0].EventID
	ops := userCtx("ops")

	if err := r.Discard(context.Background(), streamOf("t1", held), failed); !errors.Is(err, eventstore.ErrNoActor) {
		t.Errorf("Discard without actor = %v, want ErrNoActor", err)
	}
	if err := r.Discard(ops, streamOf("t1", held), uuid.Must(uuid.NewV7())); !errors.Is(err, reactor.ErrNotParked) {
		t.Errorf("Discard of an unknown event = %v, want ErrNotParked", err)
	}
	if err := r.Discard(ops, streamOf("t1", held), failed); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if err := r.Discard(ops, streamOf("t1", held), failed); !errors.Is(err, reactor.ErrNotParked) {
		t.Errorf("second Discard = %v, want ErrNotParked", err)
	}
	if got := h.notesOf(h.parked(r, "t1")); !slices.Equal(got, []string{"a", "fail"}) {
		t.Fatalf("parked after Discard %v, want [a fail] (a held, then the single Stream)", got)
	}
	res, err := r.Redrive(ops, streamOf("t1", held))
	if err != nil || res != (reactor.RedriveResult{Replayed: 1}) {
		t.Fatalf("Redrive after Discard = %+v, %v, want a replayed", res, err)
	}
	if got := rec.get(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("reacted to %v, want [a]", got)
	}

	// Discarding the last held event releases the Stream.
	if err := r.Discard(ops, streamOf("t1", single), h.history("t1", single)[0].EventID); err != nil {
		t.Fatalf("Discard last: %v", err)
	}
	h.append(userCtx("t1"), single, placed{Note: "next"})
	h.drain(r)
	if got := rec.get(); !slices.Equal(got, []string{"a", "next"}) {
		t.Errorf("reacted to %v, want the released Stream to flow live", got)
	}
	if got := h.parked(r, ""); len(got) != 0 {
		t.Errorf("parked = %+v, want none", got)
	}

	if h.backend.Discarded == nil {
		return
	}
	audit, err := h.backend.Discarded(context.Background(), "discard")
	if err != nil {
		t.Fatalf("Discarded: %v", err)
	}
	if len(audit) != 2 || audit[0].EventID != failed || audit[0].By != "user-1" || audit[1].By != "user-1" {
		t.Errorf("audit = %+v, want both discarded events by user-1", audit)
	}
}

func (s suite) parkTwiceIsNoop(t *testing.T) {
	h := s.setup(t)
	id := h.place(userCtx("t1"), "fail", "a")

	var calls atomic.Int32
	r := h.newReactor(reactor.Config{Name: "twice", Handler: script(&reactions{}, func(context.Context, string) error {
		calls.Add(1)
		return errHopeless
	})})
	h.drain(r)
	before := h.parked(r, "t1")

	for _, env := range h.history("t1", id) {
		if err := h.backend.Redeliver(context.Background(), r, env); err != nil {
			t.Fatalf("Redeliver: %v", err)
		}
	}
	if got := h.parked(r, "t1"); !slices.Equal(got, before) {
		t.Errorf("parked after redelivery = %+v, want unchanged %+v", got, before)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("reactions = %d, want 1", got)
	}
}
