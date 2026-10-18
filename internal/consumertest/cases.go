package consumertest

import (
	"context"
	"errors"
	"log/slog"
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
)

type testCase struct {
	name string
	fn   func(t *testing.T)
}

func cases(s suite) []testCase {
	return []testCase{
		{"TransactionalExactlyOnce", s.transactionalExactlyOnce},
		{"RetryThenStopped/Transactional", s.retryThenStopped(consumer.Transactional)},
		{"RetryThenStopped/OwnTransaction", s.retryThenStopped(consumer.OwnTransaction)},
		{"StopsImmediately/Permanent", s.stopsImmediately(added{Name: "a"}, func() error {
			return consumer.Permanent(errors.New("hopeless"))
		})},
		{"StopsImmediately/Panic", s.stopsImmediately(added{Name: "a"}, func() error { panic("kaboom") })},
		{"StopsImmediately/DecodeError", s.stopsImmediately(poisoned{Value: "x"}, nil)},
		{"ResumeContinuesFromCursor", s.resumeContinuesFromCursor},
		{"HandlerContext", s.handlerContext},
		{"FilterLimitsDelivery", s.filterLimitsDelivery},
		{"UnroutedTypesSkipped", s.unroutedTypesSkipped},
		{"DrainStopsAtHead", s.drainStopsAtHead},
		{"Status", s.status},
		{"Run", s.run},
		{"StoppedDoesNotEndRun", s.stoppedDoesNotEndRun},
		{"GroupStopsOnCancel", s.groupStopsOnCancel},
		{"Names", s.names},
		{"Await/ZeroIsFresh", s.awaitZeroIsFresh},
		{"Await/FreshAfterDrain", s.awaitFreshAfterDrain},
		{"Await/StaleStopped", s.awaitStaleStopped},
		{"Await/DefaultBudget", s.awaitDefaultBudget},
		{"Await/SkipsNonMatchingEvents", s.awaitSkipsNonMatchingEvents},
		{"Await/WaitsForEarlierTransaction", s.awaitWaitsForEarlierTransaction},
		{"Await/WhileRunning", s.awaitWhileRunning},
	}
}

type suite struct {
	newBackend func(t *testing.T, codec *eventstore.Codec) Backend
	synctest   bool
}

type harness struct {
	t       *testing.T
	backend Backend
	codec   *eventstore.Codec
	items   *eventstore.Repository[itemID, item, *item]
}

func (s suite) setup(t *testing.T) harness {
	t.Helper()
	codec := newCodec()
	b := s.newBackend(t, codec)
	return harness{
		t:       t,
		backend: b,
		codec:   codec,
		items: eventstore.NewRepository[itemID, item](eventstore.RepositoryConfig{
			Store: b.Store, UnitOfWork: b.UnitOfWork,
		}),
	}
}

func userCtx(tenant string) context.Context {
	ctx := ctxutil.WithTenantID(context.Background(), tenant)
	return ctxutil.WithClaims(ctx, ctxutil.Claims{UserID: "user-1"})
}

// write appends events to a new Stream and returns the Cursor of the last.
func (h harness) write(ctx context.Context, events ...aggregate.Event) eventstore.Cursor {
	h.t.Helper()
	res, err := h.items.Create(ctx, itemID(uuid.Must(uuid.NewV7())), func(_ context.Context, i *item) error {
		for _, e := range events {
			aggregate.Raise(i, e)
		}
		return nil
	})
	if err != nil {
		h.t.Fatalf("write: %v", err)
	}
	return res.Position
}

func (h harness) newConsumer(cfg consumer.Config, opts ...consumer.Option) *consumer.Consumer {
	h.t.Helper()
	opts = append([]consumer.Option{fastRetry, quiet, consumer.WithPollInterval(10 * time.Millisecond)}, opts...)
	c, err := consumer.New(h.backend.Runtime, cfg, opts...)
	if err != nil {
		h.t.Fatalf("consumer.New: %v", err)
	}
	return c
}

var quiet = consumer.WithLogger(slog.New(slog.DiscardHandler))

var fastRetry = consumer.WithRetry(consumer.Retry{MaxAttempts: 3, Base: time.Millisecond, Max: 2 * time.Millisecond})

func (h harness) keys() []string {
	h.t.Helper()
	keys, err := h.backend.Effects.Keys(context.Background())
	if err != nil {
		h.t.Fatalf("Effects.Keys: %v", err)
	}
	return keys
}

func (h harness) status(c *consumer.Consumer) consumer.Status {
	h.t.Helper()
	st, err := c.Status(context.Background())
	if err != nil {
		h.t.Fatalf("Status: %v", err)
	}
	return st
}

// eventIDs collects the EventIDs a handler saw.
type eventIDs struct {
	mu  sync.Mutex
	ids []string
}

func (e *eventIDs) add(env eventstore.Envelope) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, env.EventID.String())
}

func (e *eventIDs) get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.ids)
}

func (s suite) transactionalExactlyOnce(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.write(ctx, added{Name: "a"})
	second := h.write(ctx, added{Name: "b"})
	last := h.write(ctx, added{Name: "c"})

	var seen eventIDs
	var failedOnce atomic.Bool
	c := h.newConsumer(consumer.Config{
		Name: "exactly_once",
		Mode: consumer.Transactional,
		Handler: consumer.HandlerFunc(func(ctx context.Context, env eventstore.Envelope) error {
			if err := h.backend.Effects.Record(ctx, env.EventID.String()); err != nil {
				return err
			}
			if env.Cursor == second && failedOnce.CompareAndSwap(false, true) {
				return errors.New("transient")
			}
			seen.add(env)
			return nil
		}),
	})

	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	got := h.keys()
	// seen also holds handler runs that rolled back; the committed ones are
	// the last three.
	if want := seen.get(); len(got) != 3 || len(want) < 3 || !slices.Equal(got, want[len(want)-3:]) {
		t.Fatalf("effects = %v, want each event once in order: %v", got, want)
	}
	if st := h.status(c); st.Cursor != last || st.Pending != 0 || st.Stopped != nil {
		t.Fatalf("Status = %+v, want cursor %v, nothing pending, not stopped", st, last)
	}
}

// retryThenStopped retries a failing event, then stops before it. In
// Transactional mode the failing event's effect rolls back.
func (s suite) retryThenStopped(mode consumer.Mode) func(t *testing.T) {
	return func(t *testing.T) {
		h := s.setup(t)
		ctx := userCtx("t1")
		first := h.write(ctx, added{Name: "a"})
		poison := h.write(ctx, added{Name: "b"})
		h.write(ctx, added{Name: "c"})

		var attempts atomic.Int32
		c := h.newConsumer(consumer.Config{
			Name: "retry",
			Mode: mode,
			Handler: consumer.HandlerFunc(func(ctx context.Context, env eventstore.Envelope) error {
				if err := h.backend.Effects.Record(ctx, env.EventID.String()); err != nil {
					return err
				}
				if env.Cursor == poison {
					attempts.Add(1)
					return errors.New("boom")
				}
				return nil
			}),
		})

		err := c.Drain(context.Background())
		if !errors.Is(err, consumer.ErrStopped) {
			t.Fatalf("Drain = %v, want ErrStopped", err)
		}
		if got := attempts.Load(); got != 3 {
			t.Errorf("attempts = %d, want 3", got)
		}
		st := h.status(c)
		if st.Cursor != first || st.Stopped == nil || !strings.Contains(st.Stopped.Cause, "boom") {
			t.Fatalf("Status = %+v, want cursor %v and Stopped by boom", st, first)
		}
		if st.Pending != 2 {
			t.Errorf("Pending = %d, want 2", st.Pending)
		}
		if keys := h.keys(); mode == consumer.Transactional && len(keys) != 1 {
			t.Errorf("effects = %v, want only the first event's", keys)
		}
		if err := c.Drain(context.Background()); !errors.Is(err, consumer.ErrStopped) {
			t.Errorf("Drain while Stopped = %v, want ErrStopped", err)
		}
		if got := attempts.Load(); got != 3 {
			t.Errorf("attempts after second Drain = %d, want 3", got)
		}
	}
}

// stopsImmediately writes event and stops on the first failure of fail
// without retrying. A nil fail expects event never to reach the handler.
func (s suite) stopsImmediately(event aggregate.Event, fail func() error) func(t *testing.T) {
	return func(t *testing.T) {
		h := s.setup(t)
		h.write(userCtx("t1"), event)

		var calls atomic.Int32
		c := h.newConsumer(consumer.Config{
			Name: "stops",
			Mode: consumer.Transactional,
			Handler: consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error {
				calls.Add(1)
				if fail == nil {
					return nil
				}
				return fail()
			}),
		})

		if err := c.Drain(context.Background()); !errors.Is(err, consumer.ErrStopped) {
			t.Fatalf("Drain = %v, want ErrStopped", err)
		}
		want := int32(1)
		if fail == nil {
			want = 0
		}
		if got := calls.Load(); got != want {
			t.Errorf("handler calls = %d, want %d", got, want)
		}
		if st := h.status(c); st.Stopped == nil || !st.Cursor.IsZero() {
			t.Errorf("Status = %+v, want Stopped at the zero Cursor", st)
		}
	}
}

func (s suite) resumeContinuesFromCursor(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.write(ctx, added{Name: "a"})
	poison := h.write(ctx, added{Name: "b"})
	last := h.write(ctx, added{Name: "c"})

	var broken atomic.Bool
	broken.Store(true)
	c := h.newConsumer(consumer.Config{
		Name: "resume",
		Mode: consumer.Transactional,
		Handler: consumer.HandlerFunc(func(ctx context.Context, env eventstore.Envelope) error {
			if env.Cursor == poison && broken.Load() {
				return consumer.Permanent(errors.New("broken"))
			}
			return h.backend.Effects.Record(ctx, env.EventID.String())
		}),
	})
	if err := c.Drain(context.Background()); !errors.Is(err, consumer.ErrStopped) {
		t.Fatalf("Drain = %v, want ErrStopped", err)
	}

	broken.Store(false)
	if err := h.backend.Runtime.Resume(context.Background(), "resume"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain after Resume: %v", err)
	}
	if keys := h.keys(); len(keys) != 3 {
		t.Errorf("effects = %v, want 3 events once each", keys)
	}
	if st := h.status(c); st.Cursor != last || st.Stopped != nil {
		t.Errorf("Status = %+v, want cursor %v and not Stopped", st, last)
	}
}

func (s suite) handlerContext(t *testing.T) {
	h := s.setup(t)
	h.write(ctxutil.WithCorrelationID(userCtx("t2"), "corr-1"), added{Name: "a"})

	var tenant, correlation string
	c := h.newConsumer(consumer.Config{
		Name: "context",
		Mode: consumer.OwnTransaction,
		Handler: consumer.HandlerFunc(func(ctx context.Context, _ eventstore.Envelope) error {
			tenant, _ = ctxutil.TenantID(ctx)
			correlation, _ = ctxutil.CorrelationID(ctx)
			return nil
		}),
	})
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if tenant != "t2" || correlation != "corr-1" {
		t.Errorf("handler ctx tenant %q correlation %q, want t2 corr-1", tenant, correlation)
	}
}

func (s suite) filterLimitsDelivery(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.write(ctx, added{Name: "a"})
	lastAdded := h.write(ctx, added{Name: "b"})
	h.write(ctx, renamed{Name: "c"}, poisoned{Value: "never read"})

	var names []string
	r := consumer.NewRouter(h.codec)
	consumer.On(r, func(_ context.Context, _ eventstore.Envelope, e added) error {
		names = append(names, e.Name)
		return nil
	})
	c := h.newConsumer(consumer.Config{Name: "filtered", Mode: consumer.Transactional, Handler: r})

	if st := h.status(c); st.Pending != 2 {
		t.Errorf("Pending before Drain = %d, want 2", st.Pending)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !slices.Equal(names, []string{"a", "b"}) {
		t.Errorf("handled %v, want [a b]", names)
	}
	if st := h.status(c); st.Cursor != lastAdded || st.Pending != 0 {
		t.Errorf("Status = %+v, want cursor at the last matching event %v, nothing pending", st, lastAdded)
	}
}

func (s suite) unroutedTypesSkipped(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.write(ctx, added{Name: "a"})
	last := h.write(ctx, renamed{Name: "b"})

	var calls int
	r := consumer.NewRouter(h.codec)
	consumer.On(r, func(context.Context, eventstore.Envelope, added) error {
		calls++
		return nil
	})
	c := h.newConsumer(consumer.Config{
		Name:       "unrouted",
		Mode:       consumer.Transactional,
		EventTypes: []string{added{}.EventType(), renamed{}.EventType()},
		Handler:    r,
	})
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if calls != 1 {
		t.Errorf("routed calls = %d, want 1", calls)
	}
	if st := h.status(c); st.Cursor != last || st.Stopped != nil {
		t.Errorf("Status = %+v, want cursor past the unrouted event %v", st, last)
	}
}

func (s suite) drainStopsAtHead(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.write(ctx, added{Name: "a"})
	head := h.write(ctx, added{Name: "b"})

	var calls int
	c := h.newConsumer(consumer.Config{
		Name: "head",
		Mode: consumer.OwnTransaction,
		Handler: consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error {
			calls++
			h.write(userCtx("t1"), added{Name: "later"})
			return nil
		}),
	})
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if calls != 2 {
		t.Errorf("handled %d events, want the 2 up to the head", calls)
	}
	if st := h.status(c); st.Cursor != head || st.Pending != 2 {
		t.Errorf("Status = %+v, want cursor %v and the 2 later events pending", st, head)
	}
}

func (s suite) status(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.write(ctx, added{Name: "a"})
	last := h.write(ctx, added{Name: "b"})
	if s.synctest {
		time.Sleep(5 * time.Second)
	}

	c := h.newConsumer(consumer.Config{
		Name:    "status",
		Mode:    consumer.Transactional,
		Handler: consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error { return nil }),
	})
	st := h.status(c)
	if !st.Cursor.IsZero() || st.Pending != 2 || st.Stopped != nil {
		t.Errorf("Status before Drain = %+v, want zero cursor, 2 pending", st)
	}
	if s.synctest && st.Age != 5*time.Second {
		t.Errorf("Age = %s, want 5s", st.Age)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if st := h.status(c); st != (consumer.Status{Cursor: last}) {
		t.Errorf("Status after Drain = %+v, want cursor %v only", st, last)
	}
}

// signal returns a handler that sends each EventID on a channel.
func signal() (consumer.Handler, <-chan uuid.UUID) {
	ch := make(chan uuid.UUID, 16)
	return consumer.HandlerFunc(func(_ context.Context, env eventstore.Envelope) error {
		ch <- env.EventID
		return nil
	}), ch
}

func receive(t *testing.T, ch <-chan uuid.UUID) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("handler not called")
	}
}

func runInBackground(c interface{ Run(context.Context) error }) (cancel func() error) {
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	return func() error {
		stop()
		return <-done
	}
}

func (s suite) run(t *testing.T) {
	h := s.setup(t)
	handler, handled := signal()
	c := h.newConsumer(consumer.Config{Name: "run", Mode: consumer.OwnTransaction, Handler: handler})

	stop := runInBackground(c)
	last := h.write(userCtx("t1"), added{Name: "a"})
	receive(t, handled)

	if err := c.Run(context.Background()); !errors.Is(err, consumer.ErrRunning) {
		t.Errorf("second Run = %v, want ErrRunning", err)
	}
	if err := c.Drain(context.Background()); !errors.Is(err, consumer.ErrRunning) {
		t.Errorf("Drain while running = %v, want ErrRunning", err)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}
	if st := h.status(c); st.Cursor != last {
		t.Errorf("Cursor = %v, want %v", st.Cursor, last)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Errorf("Drain after Run = %v, want nil", err)
	}
}

func (s suite) stoppedDoesNotEndRun(t *testing.T) {
	h := s.setup(t)
	var broken atomic.Bool
	broken.Store(true)
	attempts := make(chan uuid.UUID, 16)
	c := h.newConsumer(consumer.Config{
		Name: "stopped_run",
		Mode: consumer.Transactional,
		Handler: consumer.HandlerFunc(func(_ context.Context, env eventstore.Envelope) error {
			attempts <- env.EventID
			if broken.Load() {
				return consumer.Permanent(errors.New("broken"))
			}
			return nil
		}),
	})

	stop := runInBackground(c)
	last := h.write(userCtx("t1"), added{Name: "a"})
	receive(t, attempts)
	eventually(t, func() bool { return h.status(c).Stopped != nil })

	broken.Store(false)
	if err := h.backend.Runtime.Resume(context.Background(), "stopped_run"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	receive(t, attempts)
	eventually(t, func() bool { return h.status(c).Cursor == last })
	if err := stop(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

func (s suite) groupStopsOnCancel(t *testing.T) {
	h := s.setup(t)
	ha, handledA := signal()
	hb, handledB := signal()
	a := h.newConsumer(consumer.Config{Name: "a", Mode: consumer.OwnTransaction, Handler: ha})
	b := h.newConsumer(consumer.Config{Name: "b", Mode: consumer.Transactional, Handler: hb})

	stop := runInBackground(consumer.Group{a, b})
	h.write(userCtx("t1"), added{Name: "a"})
	receive(t, handledA)
	receive(t, handledB)
	if err := stop(); err != nil {
		t.Fatalf("Group.Run = %v, want nil", err)
	}
	for _, c := range []*consumer.Consumer{a, b} {
		if err := c.Drain(context.Background()); err != nil {
			t.Errorf("Drain %s after Group.Run = %v, want nil (lease released)", c.Name(), err)
		}
	}
}

func (s suite) names(t *testing.T) {
	h := s.setup(t)
	handler := consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error { return nil })
	h.newConsumer(consumer.Config{Name: "dup", Mode: consumer.Transactional, Handler: handler})
	_, err := consumer.New(h.backend.Runtime, consumer.Config{Name: "dup", Mode: consumer.OwnTransaction, Handler: handler})
	if !errors.Is(err, consumer.ErrDuplicate) {
		t.Errorf("second New = %v, want ErrDuplicate", err)
	}
	if err := h.backend.Runtime.Resume(context.Background(), "missing"); !errors.Is(err, consumer.ErrNotFound) {
		t.Errorf("Resume unknown = %v, want ErrNotFound", err)
	}
	if err := h.backend.Runtime.Resume(context.Background(), "dup"); err != nil {
		t.Errorf("Resume not Stopped = %v, want nil", err)
	}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s suite) awaitZeroIsFresh(t *testing.T) {
	h := s.setup(t)
	h.write(userCtx("t1"), added{Name: "a"})
	c := h.newConsumer(consumer.Config{Name: "await_zero", Mode: consumer.Transactional, Handler: consumer.HandlerFunc(noop)})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := c.Await(ctx, eventstore.Cursor{}); got != consumer.Fresh {
		t.Errorf("Await(zero) = %v, want Fresh", got)
	}
}

func (s suite) awaitFreshAfterDrain(t *testing.T) {
	h := s.setup(t)
	pos := h.write(userCtx("t1"), added{Name: "a"})
	c := h.newConsumer(consumer.Config{Name: "await_drain", Mode: consumer.Transactional, Handler: consumer.HandlerFunc(noop)})

	if got := await(c, pos, 50*time.Millisecond); got != consumer.Stale {
		t.Errorf("Await before Drain = %v, want Stale", got)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got := await(c, pos, 10*time.Second); got != consumer.Fresh {
		t.Errorf("Await after Drain = %v, want Fresh", got)
	}
}

func (s suite) awaitStaleStopped(t *testing.T) {
	h := s.setup(t)
	pos := h.write(userCtx("t1"), added{Name: "a"})
	c := h.newConsumer(consumer.Config{
		Name: "await_stopped",
		Mode: consumer.Transactional,
		Handler: consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error {
			return consumer.Permanent(errors.New("broken"))
		}),
	})
	if err := c.Drain(context.Background()); !errors.Is(err, consumer.ErrStopped) {
		t.Fatalf("Drain = %v, want ErrStopped", err)
	}

	start := time.Now()
	if got := await(c, pos, time.Minute); got != consumer.StaleStopped {
		t.Errorf("Await = %v, want StaleStopped", got)
	}
	if elapsed := time.Since(start); elapsed >= 10*time.Second {
		t.Errorf("Await took %s, want it to return before the budget", elapsed)
	}
}

func (s suite) awaitDefaultBudget(t *testing.T) {
	h := s.setup(t)
	pos := h.write(userCtx("t1"), added{Name: "a"})
	c := h.newConsumer(consumer.Config{Name: "await_budget", Mode: consumer.Transactional, Handler: consumer.HandlerFunc(noop)})

	start := time.Now()
	if got := c.Await(context.Background(), pos); got != consumer.Stale {
		t.Errorf("Await = %v, want Stale", got)
	}
	elapsed := time.Since(start)
	if s.synctest && elapsed != 2*time.Second || elapsed < 2*time.Second || elapsed > 5*time.Second {
		t.Errorf("Await without deadline took %s, want 2s", elapsed)
	}
}

// awaitSkipsNonMatchingEvents: a filtered Consumer is Fresh at its last
// matching event even when pos is a later event of another type.
func (s suite) awaitSkipsNonMatchingEvents(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.write(ctx, added{Name: "a"})
	other := h.write(ctx, renamed{Name: "b"})
	c := h.newConsumer(consumer.Config{
		Name:       "await_filtered",
		Mode:       consumer.Transactional,
		EventTypes: []string{added{}.EventType()},
		Handler:    consumer.HandlerFunc(noop),
	})

	if got := await(c, other, 50*time.Millisecond); got != consumer.Stale {
		t.Errorf("Await before Drain = %v, want Stale (a matching event is pending)", got)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got := await(c, other, 10*time.Second); got != consumer.Fresh {
		t.Errorf("Await at the last matching event = %v, want Fresh", got)
	}

	h.write(ctx, renamed{Name: "c"})
	matching := h.write(ctx, added{Name: "d"})
	if got := await(c, matching, 50*time.Millisecond); got != consumer.Stale {
		t.Errorf("Await on a new matching event = %v, want Stale", got)
	}
}

// awaitWaitsForEarlierTransaction: an open transaction that started before
// pos may still commit a matching event below pos, so pos is not covered
// until it ends.
func (s suite) awaitWaitsForEarlierTransaction(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	c := h.newConsumer(consumer.Config{
		Name:       "await_open_tx",
		Mode:       consumer.Transactional,
		EventTypes: []string{added{}.EventType()},
		Handler:    consumer.HandlerFunc(noop),
	})

	early := itemID(uuid.Must(uuid.NewV7()))
	appended := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- h.backend.UnitOfWork.Execute(ctx, uuid.UUID(early), func(ctx context.Context) error {
			_, err := h.items.Create(ctx, early, func(_ context.Context, i *item) error {
				aggregate.Raise(i, added{Name: "early"})
				return nil
			})
			close(appended)
			<-release
			return err
		})
	}()
	<-appended
	pos := h.write(ctx, renamed{Name: "late"})

	if got := await(c, pos, 50*time.Millisecond); got != consumer.Stale {
		t.Errorf("Await while an earlier transaction is open = %v, want Stale", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("early transaction: %v", err)
	}
	if got := await(c, pos, 50*time.Millisecond); got != consumer.Stale {
		t.Errorf("Await with the early event pending = %v, want Stale", got)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got := await(c, pos, 10*time.Second); got != consumer.Fresh {
		t.Errorf("Await after Drain = %v, want Fresh", got)
	}
}

func (s suite) awaitWhileRunning(t *testing.T) {
	h := s.setup(t)
	c := h.newConsumer(consumer.Config{Name: "await_run", Mode: consumer.OwnTransaction, Handler: consumer.HandlerFunc(noop)})
	stop := runInBackground(c)
	t.Cleanup(func() { _ = stop() })

	pos := h.write(userCtx("t1"), added{Name: "a"})
	if got := await(c, pos, 10*time.Second); got != consumer.Fresh {
		t.Errorf("Await = %v, want Fresh", got)
	}
}

// await calls c.Await with a budget of d.
func await(c *consumer.Consumer, pos eventstore.Cursor, d time.Duration) consumer.Freshness {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return c.Await(ctx, pos)
}

func noop(context.Context, eventstore.Envelope) error { return nil }
