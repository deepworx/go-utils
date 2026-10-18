package projection_test

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
	"github.com/deepworx/go-utils/pkg/eventsourcing/projection"
	"github.com/deepworx/go-utils/pkg/postgres"
)

type noteID uuid.UUID

type noteWritten struct {
	Text string `json:"text"`
}

func (noteWritten) EventType() string { return "Test.Note.Written" }

type note struct {
	aggregate.Base[noteID]
}

func (n *note) StreamType() string { return "Test.Note" }

func (n *note) Apply(aggregate.Event) {}

func newCodec() *eventstore.Codec {
	c := eventstore.NewCodec()
	eventstore.Register[noteWritten](c)
	return c
}

var quiet = consumer.WithLogger(slog.New(slog.DiscardHandler))

var fastRetry = consumer.WithRetry(consumer.Retry{MaxAttempts: 1, Base: time.Millisecond, Max: time.Millisecond})

// readModel holds one table of note texts per Projection version.
type readModel interface {
	// Add joins the Cursor transaction in ctx.
	Add(ctx context.Context, version int, text string) error
	Texts(ctx context.Context, version int) ([]string, error)
	// Clear empties a version's table, joining the transaction in ctx.
	Clear(ctx context.Context, version int) error
}

// backend is one empty event log with a Runtime and a read model over it.
type backend struct {
	store   eventstore.Store
	uow     eventstore.UnitOfWork
	runtime consumer.Runtime
	model   readModel
	// subscribe returns a Subscription that is known to be listening.
	subscribe func(t *testing.T) *projection.Subscription
}

type harness struct {
	backend
	codec *eventstore.Codec
	notes *eventstore.Repository[noteID, note, *note]
}

func newHarness(t *testing.T, newBackend func(t *testing.T, codec *eventstore.Codec) backend) harness {
	t.Helper()
	codec := newCodec()
	b := newBackend(t, codec)
	return harness{
		backend: b,
		codec:   codec,
		notes: eventstore.NewRepository[noteID, note](eventstore.RepositoryConfig{
			Store: b.store, UnitOfWork: b.uow,
		}),
	}
}

func writerCtx() context.Context {
	return ctxutil.WithActor(ctxutil.WithTenantID(context.Background(), "t1"), "tester")
}

// write appends one note and returns its ID and Position.
func (h harness) write(t *testing.T, text string) (uuid.UUID, eventstore.Cursor) {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	res, err := h.notes.Create(writerCtx(), noteID(id), func(_ context.Context, n *note) error {
		aggregate.Raise(n, noteWritten{Text: text})
		return nil
	})
	if err != nil {
		t.Fatalf("write %q: %v", text, err)
	}
	return id, res.Position
}

type handlerInput struct {
	version int
	// entity is what the handler notifies about.
	entity string
	// failOn makes the handler fail on a note with this text, after it
	// added the note and notified.
	failOn string
}

// handler adds each note to the version's table and notifies about it.
func (h harness) handler(in handlerInput) consumer.Handler {
	r := consumer.NewRouter(h.codec)
	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, e noteWritten) error {
		if err := h.model.Add(ctx, in.version, e.Text); err != nil {
			return err
		}
		if err := projection.Notify(ctx, in.entity, env.StreamID); err != nil {
			return err
		}
		if in.failOn != "" && e.Text == in.failOn {
			return consumer.Permanent(errors.New("fail"))
		}
		return nil
	})
	return r
}

func (h harness) version(v int) projection.Version {
	return projection.Version{
		N:       v,
		Handler: h.handler(handlerInput{version: v, entity: "note"}),
		Clear:   func(ctx context.Context) error { return h.model.Clear(ctx, v) },
	}
}

func (h harness) newProjection(t *testing.T, active int, shadow int) *projection.Projection {
	t.Helper()
	cfg := projection.Config{Name: "notes", Active: h.version(active)}
	if shadow > 0 {
		s := h.version(shadow)
		cfg.Shadow = &s
	}
	p, err := projection.New(h.runtime, cfg, quiet, fastRetry, consumer.WithPollInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func (h harness) texts(t *testing.T, v int) []string {
	t.Helper()
	texts, err := h.model.Texts(t.Context(), v)
	if err != nil {
		t.Fatalf("texts v%d: %v", v, err)
	}
	return texts
}

func drain(t *testing.T, c *consumer.Consumer) {
	t.Helper()
	if err := c.Drain(t.Context()); err != nil {
		t.Fatalf("drain %s: %v", c.Name(), err)
	}
}

func caughtUp(t *testing.T, p *projection.Projection) bool {
	t.Helper()
	ok, err := p.CaughtUp(t.Context())
	if err != nil {
		t.Fatalf("CaughtUp: %v", err)
	}
	return ok
}

func await(t *testing.T, p *projection.Projection, pos eventstore.Cursor) consumer.Freshness {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	return p.Await(ctx, pos)
}

const receiveTimeout = 5 * time.Second

func receive(t *testing.T, sub *projection.Subscription) projection.Change {
	t.Helper()
	select {
	case c, ok := <-sub.C():
		if !ok {
			t.Fatal("subscription closed")
		}
		return c
	case <-time.After(receiveTimeout):
		t.Fatal("no change received")
	}
	return projection.Change{}
}

type contractCase struct {
	name string
	fn   func(t *testing.T, h harness)
}

func contractCases() []contractCase {
	return []contractCase{
		{"VersionsBuildIndependently", versionsBuildIndependently},
		{"CaughtUpWithoutShadow", caughtUpWithoutShadow},
		{"AwaitTargetsActive", awaitTargetsActive},
		{"NotifyAfterCommit", notifyAfterCommit},
		{"NotifyNotSentOnFailure", notifyNotSentOnFailure},
		{"NotifyNeverFromShadow", notifyNeverFromShadow},
		{"ResetClearsAndRewinds", resetClearsAndRewinds},
		{"ResetIsAtomic", resetIsAtomic},
		{"ResetWhileRunning", resetWhileRunning},
	}
}

func runContract(t *testing.T, newBackend func(t *testing.T, codec *eventstore.Codec) backend, parallel bool) {
	t.Helper()
	for _, tc := range contractCases() {
		t.Run(tc.name, func(t *testing.T) {
			if parallel {
				t.Parallel()
			}
			tc.fn(t, newHarness(t, newBackend))
		})
	}
}

func versionsBuildIndependently(t *testing.T, h harness) {
	h.write(t, "a")
	h.write(t, "b")
	p := h.newProjection(t, 1, 2)

	names := []string{}
	for _, c := range p.Consumers() {
		names = append(names, c.Name())
	}
	if want := []string{"notes@v1", "notes@v2"}; !slices.Equal(names, want) {
		t.Fatalf("Consumers = %v, want %v", names, want)
	}
	active, shadow := p.Consumers()[0], p.Consumers()[1]

	drain(t, active)
	if got := h.texts(t, 1); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("v1 = %v, want [a b]", got)
	}
	if got := h.texts(t, 2); len(got) != 0 {
		t.Errorf("v2 = %v before its drain, want empty", got)
	}
	if caughtUp(t, p) {
		t.Error("CaughtUp = true while the shadow lags")
	}

	drain(t, shadow)
	if got := h.texts(t, 2); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("v2 = %v, want [a b]", got)
	}
	if !caughtUp(t, p) {
		t.Error("CaughtUp = false after the shadow drained")
	}
}

func caughtUpWithoutShadow(t *testing.T, h harness) {
	h.write(t, "a")
	p := h.newProjection(t, 1, 0)
	if n := len(p.Consumers()); n != 1 {
		t.Fatalf("len(Consumers) = %d, want 1", n)
	}
	if caughtUp(t, p) {
		t.Error("CaughtUp = true while the active version lags")
	}
	drain(t, p.Consumers()[0])
	if !caughtUp(t, p) {
		t.Error("CaughtUp = false after the active version drained")
	}
}

func awaitTargetsActive(t *testing.T, h harness) {
	_, pos := h.write(t, "a")
	p := h.newProjection(t, 1, 2)
	active, shadow := p.Consumers()[0], p.Consumers()[1]

	drain(t, shadow)
	if got := await(t, p, pos); got != consumer.Stale {
		t.Errorf("Await with only the shadow drained = %v, want Stale", got)
	}
	drain(t, active)
	if got := await(t, p, pos); got != consumer.Fresh {
		t.Errorf("Await after the active drained = %v, want Fresh", got)
	}
}

func notifyAfterCommit(t *testing.T, h harness) {
	sub := h.subscribe(t)
	id, _ := h.write(t, "a")
	p := h.newProjection(t, 1, 0)

	drain(t, p.Consumers()[0])
	want := projection.Change{Tenant: "t1", Entity: "note", ID: id}
	if got := receive(t, sub); got != want {
		t.Errorf("change = %+v, want %+v", got, want)
	}
}

func notifyNotSentOnFailure(t *testing.T, h harness) {
	sub := h.subscribe(t)
	id, _ := h.write(t, "fail")
	broken, err := projection.New(h.runtime, projection.Config{Name: "broken", Active: projection.Version{
		N:       1,
		Handler: h.handler(handlerInput{version: 1, entity: "broken", failOn: "fail"}),
	}}, quiet, fastRetry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := broken.Consumers()[0].Drain(t.Context()); !errors.Is(err, consumer.ErrStopped) {
		t.Fatalf("drain failing note = %v, want ErrStopped", err)
	}
	if got := h.texts(t, 1); len(got) != 0 {
		t.Fatalf("v1 = %v after the failed transaction, want empty", got)
	}

	// The change of a later commit must be the first one delivered.
	p := h.newProjection(t, 2, 0)
	drain(t, p.Consumers()[0])
	want := projection.Change{Tenant: "t1", Entity: "note", ID: id}
	if got := receive(t, sub); got != want {
		t.Errorf("first change = %+v, want %+v (from the later commit)", got, want)
	}
}

func notifyNeverFromShadow(t *testing.T, h harness) {
	sub := h.subscribe(t)
	h.write(t, "a")
	p := h.newProjection(t, 1, 2)
	active, shadow := p.Consumers()[0], p.Consumers()[1]

	drain(t, shadow)
	id, _ := h.write(t, "b")
	drain(t, active)

	first := receive(t, sub)
	if first.ID == id {
		t.Fatalf("first change is for b; want the active version's change for a first")
	}
	second := receive(t, sub)
	if second.ID != id {
		t.Errorf("second change = %+v, want the active version's change for b", second)
	}
}

func resetClearsAndRewinds(t *testing.T, h harness) {
	h.write(t, "a")
	h.write(t, "b")
	p := h.newProjection(t, 1, 2)
	for _, c := range p.Consumers() {
		drain(t, c)
	}

	if err := p.Reset(t.Context()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, v := range []int{1, 2} {
		if got := h.texts(t, v); len(got) != 0 {
			t.Errorf("v%d = %v after Reset, want empty", v, got)
		}
	}
	if caughtUp(t, p) {
		t.Error("CaughtUp = true after Reset rewound the Cursors")
	}

	for _, c := range p.Consumers() {
		drain(t, c)
	}
	for _, v := range []int{1, 2} {
		if got := h.texts(t, v); !slices.Equal(got, []string{"a", "b"}) {
			t.Errorf("v%d = %v after rebuilding, want [a b]", v, got)
		}
	}
}

func resetIsAtomic(t *testing.T, h harness) {
	_, pos := h.write(t, "a")
	failing := h.version(1)
	failing.Clear = func(ctx context.Context) error {
		if err := h.model.Clear(ctx, 1); err != nil {
			return err
		}
		return errors.New("clear failed")
	}
	p, err := projection.New(h.runtime, projection.Config{Name: "notes", Active: failing}, quiet, fastRetry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, p.Consumers()[0])

	if err := p.Reset(t.Context()); err == nil {
		t.Fatal("Reset with a failing Clear = nil, want an error")
	}
	if got := h.texts(t, 1); !slices.Equal(got, []string{"a"}) {
		t.Errorf("v1 = %v after the failed Reset, want [a]", got)
	}
	if got := await(t, p, pos); got != consumer.Fresh {
		t.Errorf("Await after the failed Reset = %v, want Fresh (Cursor kept)", got)
	}
}

func resetWhileRunning(t *testing.T, h harness) {
	p := h.newProjection(t, 1, 2)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	wg.Go(func() { _ = p.Consumers()[1].Run(ctx) })

	deadline := time.Now().Add(receiveTimeout)
	for {
		err := p.Reset(t.Context())
		if errors.Is(err, consumer.ErrRunning) {
			return
		}
		if err != nil {
			t.Fatalf("Reset = %v, want ErrRunning", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("Reset never reported ErrRunning while the shadow runs")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t, newInMemoryBackend)
	valid := func() projection.Config {
		return projection.Config{Name: "notes", Active: h.version(1)}
	}
	shadow := func(n int) *projection.Version {
		v := h.version(n)
		return &v
	}
	tests := []struct {
		name string
		cfg  func(c *projection.Config)
	}{
		{name: "EmptyName", cfg: func(c *projection.Config) { c.Name = "" }},
		{name: "NameWithAt", cfg: func(c *projection.Config) { c.Name = "notes@v1" }},
		{name: "ZeroActiveVersion", cfg: func(c *projection.Config) { c.Active.N = 0 }},
		{name: "NilActiveHandler", cfg: func(c *projection.Config) { c.Active.Handler = nil }},
		{name: "ShadowNotNewer", cfg: func(c *projection.Config) { c.Shadow = shadow(1) }},
		{name: "NilShadowHandler", cfg: func(c *projection.Config) {
			c.Shadow = shadow(2)
			c.Shadow.Handler = nil
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.cfg(&cfg)
			if _, err := projection.New(h.runtime, cfg); !errors.Is(err, consumer.ErrInvalidConfig) {
				t.Errorf("New = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestNotifyOutsideHandler(t *testing.T) {
	t.Parallel()
	err := projection.Notify(context.Background(), "note", uuid.New())
	if !errors.Is(err, projection.ErrOutsideHandler) {
		t.Errorf("Notify = %v, want ErrOutsideHandler", err)
	}
}

func TestResetWithoutClear(t *testing.T) {
	t.Parallel()
	h := newHarness(t, newInMemoryBackend)
	v := h.version(1)
	v.Clear = nil
	p, err := projection.New(h.runtime, projection.Config{Name: "notes", Active: v})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Reset(t.Context()); !errors.Is(err, consumer.ErrInvalidConfig) {
		t.Errorf("Reset = %v, want ErrInvalidConfig", err)
	}
}

func TestInMemoryContract(t *testing.T) {
	t.Parallel()
	runContract(t, newInMemoryBackend, true)
}

func newInMemoryBackend(_ *testing.T, codec *eventstore.Codec) backend {
	store := eventstore.NewInMemoryStore(codec)
	rt := consumer.NewInMemoryRuntime(consumer.InMemoryRuntimeConfig{Store: store})
	return backend{
		store:   store,
		uow:     eventstore.NewInMemoryUnitOfWork(),
		runtime: rt,
		model:   &memModel{tables: map[int][]string{}},
		subscribe: func(t *testing.T) *projection.Subscription {
			t.Helper()
			sub, err := projection.Subscribe(t.Context(), rt)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			t.Cleanup(sub.Close)
			return sub
		},
	}
}

type memModel struct {
	mu     sync.Mutex
	tables map[int][]string
}

func (m *memModel) Add(ctx context.Context, version int, text string) error {
	postgres.Stage(ctx, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.tables[version] = append(m.tables[version], text)
	})
	return nil
}

func (m *memModel) Texts(_ context.Context, version int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.tables[version]), nil
}

func (m *memModel) Clear(ctx context.Context, version int) error {
	postgres.Stage(ctx, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.tables, version)
	})
	return nil
}
