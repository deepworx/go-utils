package reactortest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepworx/go-utils/pkg/eventsourcing/reactor"
)

// clock is the Reactors' clock: real time, or fake time in a bubble, moved
// on by advance. It lets Pg tests reach a redrive step without sleeping.
type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// failUntilFixed fails every reaction Permanently until fixed is set.
func failUntilFixed(fixed *atomic.Bool) func(context.Context, string) error {
	return func(context.Context, string) error {
		if !fixed.Load() {
			return errHopeless
		}
		return nil
	}
}

// autoRedriveReleases: a Stream that failed once heals at the first step
// of the default schedule (1m, jittered down by up to a fifth).
func (s suite) autoRedriveReleases(t *testing.T) {
	h := s.setup(t)
	h.place(userCtx("t1"), "fail", "a")

	var (
		fixed atomic.Bool
		rec   reactions
	)
	r := h.newReactor(reactor.Config{Name: "auto", Handler: script(&rec, failUntilFixed(&fixed))})
	h.drain(r)
	fixed.Store(true)

	h.clock.advance(30 * time.Second)
	h.drain(r)
	if got := rec.get(); len(got) != 0 {
		t.Fatalf("reacted to %v before the first step, want nothing", got)
	}

	h.clock.advance(30 * time.Second)
	h.drain(r)
	if got := rec.get(); !slices.Equal(got, []string{"fail", "a"}) {
		t.Fatalf("reacted to %v at the first step, want [fail a]", got)
	}
	if got := h.parked(r, ""); len(got) != 0 {
		t.Errorf("parked = %+v, want none", got)
	}
}

func (h harness) status(r *reactor.Reactor) reactor.Status {
	h.t.Helper()
	st, err := r.Status(context.Background())
	if err != nil {
		h.t.Fatalf("Status: %v", err)
	}
	return st
}

// autoRedriveExhausts: a Stream that keeps failing is redriven once per
// step of the default schedule and then left exhausted until an operator
// redrives it.
func (s suite) autoRedriveExhausts(t *testing.T) {
	h := s.setup(t)
	id := h.place(userCtx("t1"), "fail", "a")

	var (
		fixed atomic.Bool
		calls atomic.Int32
		rec   reactions
	)
	r := h.newReactor(reactor.Config{Name: "exhausts", Handler: script(&rec, func(ctx context.Context, note string) error {
		calls.Add(1)
		return failUntilFixed(&fixed)(ctx, note)
	})})
	h.drain(r)

	steps := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}
	for i, step := range steps {
		h.clock.advance(step)
		h.drain(r)
		if got := calls.Load(); got != int32(i+2) {
			t.Fatalf("step %d: reactions = %d, want %d", i+1, got, i+2)
		}
		head := h.parked(r, "t1")[0]
		if head.Attempts != i+2 {
			t.Errorf("step %d: head attempts = %d, want %d", i+1, head.Attempts, i+2)
		}
		last := i == len(steps)-1
		if head.Exhausted != last || head.NextAttemptAt.IsZero() != last {
			t.Errorf("step %d: head = %+v, want exhausted %t", i+1, head, last)
		}
		wantExhausted := 0
		if last {
			wantExhausted = 1
		}
		if got := h.status(r).Exhausted; got != wantExhausted {
			t.Errorf("step %d: Status.Exhausted = %d, want %d", i+1, got, wantExhausted)
		}
	}

	h.clock.advance(48 * time.Hour)
	h.drain(r)
	if got := calls.Load(); got != int32(len(steps)+1) {
		t.Errorf("reactions after exhaustion = %d, want %d", got, len(steps)+1)
	}

	fixed.Store(true)
	res, err := r.Redrive(userCtx("ops"), streamOf("t1", id))
	if err != nil || res.Replayed != 2 {
		t.Fatalf("Redrive of an exhausted Stream = %+v, %v, want 2 replayed", res, err)
	}
	if st := h.status(r); st.ParkedStreams != 0 || st.Exhausted != 0 {
		t.Errorf("Status after Redrive = %+v, want nothing parked", st)
	}
}

func (s suite) noRedriveSteps(t *testing.T) {
	h := s.setup(t)
	h.place(userCtx("t1"), "fail")

	var (
		fixed atomic.Bool
		rec   reactions
	)
	r := h.newReactor(reactor.Config{Name: "manual_only", Handler: script(&rec, failUntilFixed(&fixed))},
		reactor.WithRedrive())
	h.drain(r)
	fixed.Store(true)

	h.clock.advance(48 * time.Hour)
	h.drain(r)
	if got := rec.get(); len(got) != 0 {
		t.Errorf("reacted to %v without redrive steps, want nothing", got)
	}
	parked := h.parked(r, "t1")
	if len(parked) != 1 || parked[0].Exhausted || !parked[0].NextAttemptAt.IsZero() {
		t.Errorf("parked = %+v, want one event with no schedule", parked)
	}
}

func (s suite) statusCountsParked(t *testing.T) {
	h := s.setup(t)
	h.place(userCtx("t1"), "fail", "a", "b")
	h.place(userCtx("t2"), "fail")
	h.place(userCtx("t2"), "ok")

	r := h.newReactor(reactor.Config{Name: "status", Handler: script(&reactions{}, failOn(errHopeless, "fail"))})
	if st := h.status(r); st.ParkedStreams != 0 || st.ParkedEvents != 0 || st.Pending != 5 {
		t.Errorf("Status before Drain = %+v, want 5 pending and nothing parked", st)
	}
	h.drain(r)

	st := h.status(r)
	if st.ParkedStreams != 2 || st.ParkedEvents != 4 || st.Exhausted != 0 ||
		st.Pending != 0 || st.Stopped != nil || st.Cursor.IsZero() {
		t.Errorf("Status = %+v, want 2 Streams and 4 events parked, caught up", st)
	}
}

// concurrentRedrives: an auto-redrive and a manual Redrive of one Stream
// at the same time replay each held event exactly once; the one that finds
// the head claimed by the other backs off.
func (s suite) concurrentRedrives(t *testing.T) {
	h := s.setup(t)
	id := h.place(userCtx("t1"), "fail", "a", "b", "c")

	var (
		fixed atomic.Bool
		rec   reactions
	)
	r := h.newReactor(reactor.Config{Name: "concurrent", Handler: script(&rec, failUntilFixed(&fixed))})
	h.drain(r)
	fixed.Store(true)
	h.clock.advance(time.Minute)

	var (
		wg       sync.WaitGroup
		drainErr error
		res      reactor.RedriveResult
		redrive  error
	)
	wg.Go(func() { drainErr = r.Drain(context.Background()) })
	wg.Go(func() { res, redrive = r.Redrive(userCtx("ops"), streamOf("t1", id)) })
	wg.Wait()

	if drainErr != nil {
		t.Fatalf("Drain: %v", drainErr)
	}
	if redrive != nil && !errors.Is(redrive, reactor.ErrNotParked) && !errors.Is(redrive, reactor.ErrRedriving) {
		t.Fatalf("Redrive = %+v, %v, want success, ErrNotParked or ErrRedriving", res, redrive)
	}
	if got := rec.get(); !slices.Equal(got, []string{"fail", "a", "b", "c"}) {
		t.Errorf("reacted to %v, want each event once in order", got)
	}
	if got := h.parked(r, ""); len(got) != 0 {
		t.Errorf("parked = %+v, want none", got)
	}
}

// redriveDoesNotBlockLiveHold: while a Redrive's reaction runs, the live
// handler still holds a new event of the same Stream at once, and the
// Redrive replays it too.
func (s suite) redriveDoesNotBlockLiveHold(t *testing.T) {
	h := s.setup(t)
	id := h.place(userCtx("t1"), "fail", "a")

	var (
		fixed   atomic.Bool
		rec     reactions
		started = make(chan struct{})
		gate    = make(chan struct{})
		once    sync.Once
	)
	r := h.newReactor(reactor.Config{Name: "unblocked", Handler: script(&rec, func(ctx context.Context, note string) error {
		if !fixed.Load() {
			return errHopeless
		}
		if note == "fail" {
			once.Do(func() { close(started) })
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})})
	h.drain(r)
	fixed.Store(true)

	var (
		wg      sync.WaitGroup
		res     reactor.RedriveResult
		redrive error
	)
	wg.Go(func() { res, redrive = r.Redrive(userCtx("ops"), streamOf("t1", id)) })
	<-started

	h.append(userCtx("t1"), id, placed{Note: "late"})
	late := h.history("t1", id)[2]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := h.backend.Redeliver(ctx, r, late)
	cancel()
	close(gate)
	wg.Wait()

	if err != nil {
		t.Fatalf("live handler during Redrive = %v, want the event held at once", err)
	}
	if redrive != nil || res != (reactor.RedriveResult{Replayed: 3}) {
		t.Fatalf("Redrive = %+v, %v, want 3 replayed", res, redrive)
	}
	if got := rec.get(); !slices.Equal(got, []string{"fail", "a", "late"}) {
		t.Errorf("reacted to %v, want [fail a late]", got)
	}
}

// discardDuringRedrive: while a Redrive's reaction to the head runs,
// Discard of any held event fails with ErrRedriving, so the Redrive
// replays the whole hold in order.
func (s suite) discardDuringRedrive(t *testing.T) {
	h := s.setup(t)
	id := h.place(userCtx("t1"), "fail", "a")

	var (
		fixed   atomic.Bool
		rec     reactions
		started = make(chan struct{})
		gate    = make(chan struct{})
		once    sync.Once
	)
	r := h.newReactor(reactor.Config{Name: "discard-redriving", Handler: script(&rec, func(ctx context.Context, note string) error {
		if !fixed.Load() {
			return errHopeless
		}
		if note != "fail" {
			return nil
		}
		once.Do(func() { close(started) })
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})})
	h.drain(r)
	fixed.Store(true)
	history := h.history("t1", id)
	head, next := history[0].EventID, history[1].EventID

	var (
		wg      sync.WaitGroup
		res     reactor.RedriveResult
		redrive error
	)
	wg.Go(func() { res, redrive = r.Redrive(userCtx("ops"), streamOf("t1", id)) })
	<-started
	discardHead := r.Discard(userCtx("ops"), streamOf("t1", id), head)
	discardNext := r.Discard(userCtx("ops"), streamOf("t1", id), next)
	close(gate)
	wg.Wait()

	if !errors.Is(discardHead, reactor.ErrRedriving) {
		t.Errorf("Discard of the head during Redrive = %v, want ErrRedriving", discardHead)
	}
	if !errors.Is(discardNext, reactor.ErrRedriving) {
		t.Errorf("Discard of a later event during Redrive = %v, want ErrRedriving", discardNext)
	}
	if redrive != nil || res != (reactor.RedriveResult{Replayed: 2}) {
		t.Fatalf("Redrive = %+v, %v, want 2 replayed", res, redrive)
	}
	if got := rec.get(); !slices.Equal(got, []string{"fail", "a"}) {
		t.Errorf("reacted to %v, want [fail a]", got)
	}
	if got := h.parked(r, ""); len(got) != 0 {
		t.Errorf("parked = %+v, want none", got)
	}
}
