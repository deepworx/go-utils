package inbox_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/inbox"
	"github.com/deepworx/go-utils/pkg/postgres"
)

func TestHandlerOutcomeSetsStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		result          func() error
		wantStatus      inbox.Status
		wantError       string
		wantRedelivered bool
	}{
		{name: "nil is done", result: func() error { return nil }, wantStatus: "done"},
		{name: "Ignore is ignored", result: func() error { return inbox.Ignore("unknown event") }, wantStatus: "ignored", wantError: "unknown event"},
		{name: "wrapped Ignore is ignored", result: func() error { return fmt.Errorf("check: %w", inbox.Ignore("stale")) }, wantStatus: "ignored", wantError: "stale"},
		{name: "Permanent is parked", result: func() error { return inbox.Permanent(errors.New("bad signature")) }, wantStatus: "parked", wantError: "bad signature"},
		{name: "error is retried", result: func() error { return errors.New("provider down") }, wantStatus: "pending", wantError: "provider down", wantRedelivered: true},
		{name: "panic is retried", result: func() error { panic("boom") }, wantStatus: "pending", wantError: "inbox: handler panic: boom", wantRedelivered: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clk := newClock()
			store, _ := newStore(t, inbox.WithClock(clk.Now))
			mustInsert(t, store, inbox.Message{Source: "src", Key: "k1", Type: "t"})
			rec := &recorder{}
			w := newWorker(t, store, inbox.Config{
				Source:  "src",
				Handle:  rec.handle(func(inbox.Delivery) error { return tt.result() }),
				Retries: []time.Duration{time.Minute},
			})

			drain(t, w)

			got := mustGet(t, store, "src", "k1")
			if got.Status != tt.wantStatus || got.Error != tt.wantError {
				t.Errorf("message = %+v, want status %q, error %q", got, tt.wantStatus, tt.wantError)
			}

			clk.Advance(time.Minute)
			drain(t, w)
			if redelivered := len(rec.deliveries()) == 2; redelivered != tt.wantRedelivered {
				t.Errorf("redelivered after retry delay = %v, want %v", redelivered, tt.wantRedelivered)
			}
		})
	}
}

func TestFailingMessageFollowsRetryScheduleThenParks(t *testing.T) {
	t.Parallel()

	clk := newClock()
	store, _ := newStore(t, inbox.WithClock(clk.Now))
	mustInsert(t, store, inbox.Message{Source: "src", Key: "k1", Type: "t"})
	rec := &recorder{}
	w := newWorker(t, store, inbox.Config{
		Source:  "src",
		Handle:  rec.handle(func(inbox.Delivery) error { return errors.New("still failing") }),
		Retries: []time.Duration{10 * time.Second, time.Minute},
	})

	// Jitter only shortens a step by up to a fifth, so a step is due after
	// its full delay and never before four fifths of it.
	steps := []struct {
		advance      time.Duration
		wantAttempts int
	}{
		{advance: 0, wantAttempts: 1},
		{advance: 7 * time.Second, wantAttempts: 1},
		{advance: 3 * time.Second, wantAttempts: 2},
		{advance: 47 * time.Second, wantAttempts: 2},
		{advance: 13 * time.Second, wantAttempts: 3},
		{advance: 24 * time.Hour, wantAttempts: 3},
	}
	for i, step := range steps {
		clk.Advance(step.advance)
		drain(t, w)
		if got := len(rec.deliveries()); got != step.wantAttempts {
			t.Fatalf("step %d: %d attempts, want %d", i, got, step.wantAttempts)
		}
	}
	for i, d := range rec.deliveries() {
		if d.Attempt != i+1 {
			t.Errorf("delivery %d Attempt = %d, want %d", i, d.Attempt, i+1)
		}
	}
	if got := mustGet(t, store, "src", "k1"); got.Status != "parked" || got.Attempts != 3 {
		t.Errorf("message = %+v, want parked after 3 attempts", got)
	}
}

func TestDefaultRetriesParkAfterEightAttempts(t *testing.T) {
	t.Parallel()

	clk := newClock()
	store, _ := newStore(t, inbox.WithClock(clk.Now))
	mustInsert(t, store, inbox.Message{Source: "src", Key: "k1", Type: "t"})
	rec := &recorder{}
	w := newWorker(t, store, inbox.Config{
		Source: "src",
		Handle: rec.handle(func(inbox.Delivery) error { return errors.New("fail") }),
	})

	drain(t, w)
	for _, step := range []time.Duration{2 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour} {
		clk.Advance(step)
		drain(t, w)
	}
	clk.Advance(48 * time.Hour)
	drain(t, w)

	if got := len(rec.deliveries()); got != 8 {
		t.Errorf("%d attempts, want 8", got)
	}
	if got := mustGet(t, store, "src", "k1"); got.Status != "parked" {
		t.Errorf("status = %q, want parked", got.Status)
	}
}

// blockingHandler signals started on its first call and returns result once
// release is closed.
func blockingHandler(started, release chan struct{}, result error) inbox.Handler {
	return func(context.Context, inbox.Delivery) error {
		close(started)
		<-release
		return result
	}
}

func TestSettleAfterLostClaimIsDropped(t *testing.T) {
	t.Parallel()

	clk := newClock()
	store, _ := newStore(t, inbox.WithClock(clk.Now))
	mustInsert(t, store, inbox.Message{Source: "src", Key: "k1", Type: "t"})

	started, release := make(chan struct{}), make(chan struct{})
	stale := newWorker(t, store, inbox.Config{Source: "src", Timeout: time.Minute, Handle: blockingHandler(started, release, nil)})
	staleDone := make(chan error, 1)
	go func() { staleDone <- stale.Drain(t.Context()) }()
	<-started

	// The stale claim lasts Timeout+30s; after that another Worker claims.
	clk.Advance(time.Minute + 30*time.Second)
	freshStarted, freshRelease := make(chan struct{}), make(chan struct{})
	fresh := newWorker(t, store, inbox.Config{Source: "src", Handle: blockingHandler(freshStarted, freshRelease, inbox.Ignore("handled elsewhere"))})
	freshDone := make(chan error, 1)
	go func() { freshDone <- fresh.Drain(t.Context()) }()
	<-freshStarted

	close(release)
	if err := <-staleDone; err != nil {
		t.Fatalf("stale Drain() error = %v", err)
	}
	if got := mustGet(t, store, "src", "k1"); got.Status != "pending" || got.Attempts != 2 {
		t.Fatalf("after stale settle: message = %+v, want pending at attempt 2 (stale done dropped)", got)
	}

	close(freshRelease)
	if err := <-freshDone; err != nil {
		t.Fatalf("fresh Drain() error = %v", err)
	}
	if got := mustGet(t, store, "src", "k1"); got.Status != "ignored" {
		t.Errorf("after fresh settle: status = %q, want ignored", got.Status)
	}
}

func TestLiveClaimIsExclusive(t *testing.T) {
	t.Parallel()

	clk := newClock()
	store, _ := newStore(t, inbox.WithClock(clk.Now))
	mustInsert(t, store, inbox.Message{Source: "src", Key: "k1", Type: "t"})

	started, release := make(chan struct{}), make(chan struct{})
	first := newWorker(t, store, inbox.Config{Source: "src", Timeout: time.Minute, Handle: blockingHandler(started, release, nil)})
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Drain(t.Context()) }()
	<-started

	clk.Advance(time.Minute + 29*time.Second)
	rec := &recorder{}
	drain(t, newWorker(t, store, inbox.Config{Source: "src", Handle: rec.handle(nil)}))
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Drain() error = %v", err)
	}
	if got := len(rec.deliveries()); got != 0 {
		t.Errorf("second Worker handled %d messages while the claim was live, want 0", got)
	}
}

func TestInsertRolledBackWithTransactionLeavesNoRow(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	errAbort := errors.New("abort")
	err := postgres.NewUnitOfWork(pool).Execute(t.Context(), func(ctx context.Context) error {
		if _, err := store.Insert(ctx, inbox.Message{Source: "src", Key: "k1", Type: "t"}); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("Execute() error = %v, want errAbort", err)
	}

	rec := &recorder{}
	drain(t, newWorker(t, store, inbox.Config{Source: "src", Handle: rec.handle(nil)}))
	if got := len(rec.deliveries()); got != 0 {
		t.Errorf("handled %d messages, want 0", got)
	}
	if got := len(listAll(t, store)); got != 0 {
		t.Errorf("%d rows, want 0", got)
	}
}

func TestHandlerContextCarriesMessageMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		tenant     string
		wantTenant bool
	}{
		{name: "with tenant", tenant: "acme", wantTenant: true},
		{name: "without tenant"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, _ := newStore(t)
			mustInsert(t, store, inbox.Message{Source: "github", Key: "d-1", Type: "push", Tenant: tt.tenant})
			var actor, correlation, tenant string
			var hasTenant, hasDeadline bool
			w := newWorker(t, store, inbox.Config{Source: "github", Handle: func(ctx context.Context, d inbox.Delivery) error {
				actor, _ = ctxutil.Actor(ctx)
				correlation, _ = ctxutil.CorrelationID(ctx)
				tenant, hasTenant = ctxutil.TenantID(ctx)
				_, hasDeadline = ctx.Deadline()
				return nil
			}})

			drain(t, w)

			if actor != "system:inbox:github" {
				t.Errorf("Actor = %q, want system:inbox:github", actor)
			}
			if correlation != "inbox:github:d-1" {
				t.Errorf("CorrelationID = %q, want inbox:github:d-1", correlation)
			}
			if hasTenant != tt.wantTenant || tenant != tt.tenant {
				t.Errorf("TenantID = %q, %v; want %q, %v", tenant, hasTenant, tt.tenant, tt.wantTenant)
			}
			if !hasDeadline {
				t.Error("handler ctx has no deadline, want Timeout")
			}
		})
	}
}

func TestWorkerHandlesOnlyItsSource(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	mustInsert(t, store, inbox.Message{Source: "a", Key: "k", Type: "t"})
	mustInsert(t, store, inbox.Message{Source: "b", Key: "k", Type: "t"})

	rec := &recorder{}
	drain(t, newWorker(t, store, inbox.Config{Source: "a", Handle: rec.handle(nil)}))

	if got := rec.deliveries(); len(got) != 1 || got[0].Source != "a" {
		t.Errorf("deliveries = %+v, want only source a", got)
	}
	if got := mustGet(t, store, "b", "k"); got.Status != "pending" || got.Attempts != 0 {
		t.Errorf("source b message = %+v, want untouched", got)
	}
}

func TestConcurrentWorkersHandleEveryMessageOnce(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	const n = 20
	for i := range n {
		mustInsert(t, store, inbox.Message{Source: "src", Key: fmt.Sprint(i), Type: "t"})
	}
	rec := &recorder{}
	cfg := inbox.Config{Source: "src", Concurrency: 4, Handle: rec.handle(nil)}
	workers := []*inbox.Worker{newWorker(t, store, cfg), newWorker(t, store, cfg)}

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Go(func() {
			if err := w.Drain(t.Context()); err != nil {
				t.Errorf("Drain() error = %v", err)
			}
		})
	}
	wg.Wait()

	seen := map[string]int{}
	for _, d := range rec.deliveries() {
		seen[d.Key]++
	}
	if len(seen) != n {
		t.Errorf("handled %d distinct messages, want %d", len(seen), n)
	}
	for key, count := range seen {
		if count != 1 {
			t.Errorf("message %s handled %d times, want 1", key, count)
		}
	}
}

func TestNewWorkerRejectsIncompleteConfig(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	handle := func(context.Context, inbox.Delivery) error { return nil }
	tests := []struct {
		name string
		cfg  inbox.Config
	}{
		{name: "empty source", cfg: inbox.Config{Handle: handle}},
		{name: "nil handle", cfg: inbox.Config{Source: "src"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := inbox.NewWorker(store, tt.cfg); !errors.Is(err, inbox.ErrInvalidConfig) {
				t.Errorf("NewWorker() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
