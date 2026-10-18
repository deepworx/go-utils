package inbox_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/inbox"
)

func TestListFiltersBySourceAndStatusAndHonoursLimit(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	for _, m := range []inbox.Message{
		{Source: "stripe", Key: "s1", Type: "t"},
		{Source: "stripe", Key: "s2", Type: "t"},
		{Source: "stripe", Key: "s3", Type: "t"},
		{Source: "github", Key: "g1", Type: "t"},
	} {
		mustInsert(t, store, m)
	}
	drain(t, newWorker(t, store, inbox.Config{Source: "github", Handle: (&recorder{}).handle(nil)}))

	tests := []struct {
		name     string
		filter   inbox.Filter
		wantKeys []string
	}{
		{name: "all", filter: inbox.Filter{}, wantKeys: []string{"s1", "s2", "s3", "g1"}},
		{name: "by source", filter: inbox.Filter{Source: "stripe"}, wantKeys: []string{"s1", "s2", "s3"}},
		{name: "by status", filter: inbox.Filter{Status: inbox.StatusDone}, wantKeys: []string{"g1"}},
		{name: "by source and status", filter: inbox.Filter{Source: "github", Status: inbox.StatusPending}, wantKeys: nil},
		{name: "limit", filter: inbox.Filter{Source: "stripe", Limit: 2}, wantKeys: []string{"s1", "s2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := store.List(t.Context(), tt.filter)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if keys := recordKeys(got); !slices.Equal(keys, tt.wantKeys) {
				t.Errorf("List() keys = %v, want %v", keys, tt.wantKeys)
			}
		})
	}
}

func TestRedriveReleasesParkedMessageWithAttemptsReset(t *testing.T) {
	t.Parallel()

	clk := newClock()
	store, _ := newStore(t, inbox.WithClock(clk.Now))
	mustInsert(t, store, inbox.Message{Source: "src", Key: "k1", Type: "t"})
	var fixed atomic.Bool
	rec := &recorder{}
	w := newWorker(t, store, inbox.Config{
		Source: "src",
		Handle: rec.handle(func(inbox.Delivery) error {
			if fixed.Load() {
				return nil
			}
			return errors.New("provider down")
		}),
		Retries: []time.Duration{time.Minute},
	})
	drain(t, w)
	clk.Advance(time.Minute)
	drain(t, w)
	parked := mustGet(t, store, "src", "k1")
	if parked.Status != inbox.StatusParked || parked.Attempts != 2 {
		t.Fatalf("before Redrive: %+v, want parked after 2 attempts", parked)
	}

	fixed.Store(true)
	if err := store.Redrive(t.Context(), parked.ID); err != nil {
		t.Fatalf("Redrive() error = %v", err)
	}
	drain(t, w)

	got := mustGet(t, store, "src", "k1")
	if got.Status != inbox.StatusDone || got.Attempts != 1 || got.Error != "" {
		t.Errorf("after Redrive and Drain: %+v, want done after 1 attempt without error", got)
	}
	ds := rec.deliveries()
	if len(ds) != 3 || ds[2].Attempt != 1 {
		t.Errorf("deliveries = %+v, want 3 with the last at Attempt 1", ds)
	}
}

func TestRedriveRejectsMessageThatIsNotParked(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	mustInsert(t, store, inbox.Message{Source: "src", Key: "k1", Type: "t"})
	pending := mustGet(t, store, "src", "k1")

	tests := []struct {
		name    string
		id      int64
		wantErr error
	}{
		{name: "pending", id: pending.ID, wantErr: inbox.ErrNotParked},
		{name: "unknown ID", id: pending.ID + 1000, wantErr: inbox.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := store.Redrive(t.Context(), tt.id); !errors.Is(err, tt.wantErr) {
				t.Errorf("Redrive() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestDiscardKeepsRowWithAuditFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		permanent bool
		reason    string
		wantError string
	}{
		{name: "parked with reason", permanent: true, reason: "replayed by hand", wantError: "replayed by hand"},
		{name: "parked without reason keeps last error", permanent: true, wantError: "bad signature"},
		{name: "pending", reason: "obsolete", wantError: "obsolete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clk := newClock()
			store, _ := newStore(t, inbox.WithClock(clk.Now))
			msg := inbox.Message{Source: "src", Key: "k1", Type: "t"}
			mustInsert(t, store, msg)
			if tt.permanent {
				drain(t, newWorker(t, store, inbox.Config{
					Source: "src",
					Handle: func(context.Context, inbox.Delivery) error { return inbox.Permanent(errors.New("bad signature")) },
				}))
			}
			id := mustGet(t, store, "src", "k1").ID

			clk.Advance(time.Hour)
			if err := store.Discard(userCtx(t, "alice"), id, tt.reason); err != nil {
				t.Fatalf("Discard() error = %v", err)
			}

			got := mustGet(t, store, "src", "k1")
			want := clk.Now()
			if got.Status != inbox.StatusDiscarded || got.DiscardedBy != "alice" || !got.DiscardedAt.Equal(want) ||
				!got.SettledAt.Equal(want) || got.Error != tt.wantError {
				t.Errorf("after Discard: %+v, want discarded by alice at %v with error %q", got, want, tt.wantError)
			}
			inserted, err := store.Insert(t.Context(), msg)
			if err != nil || inserted {
				t.Errorf("Insert of discarded key = %v, %v; want false, nil", inserted, err)
			}
		})
	}
}

func TestDiscardRejects(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	mustInsert(t, store, inbox.Message{Source: "src", Key: "done", Type: "t"})
	drain(t, newWorker(t, store, inbox.Config{Source: "src", Handle: (&recorder{}).handle(nil)}))
	mustInsert(t, store, inbox.Message{Source: "src", Key: "pending", Type: "t"})
	done := mustGet(t, store, "src", "done")
	pending := mustGet(t, store, "src", "pending")

	tests := []struct {
		name    string
		user    string
		id      int64
		wantErr error
	}{
		{name: "settled message", user: "alice", id: done.ID, wantErr: inbox.ErrSettled},
		{name: "unknown ID", user: "alice", id: pending.ID + 1000, wantErr: inbox.ErrNotFound},
		{name: "no actor", id: pending.ID, wantErr: ctxutil.ErrNoActor},
		{name: "reserved user prefix", user: "system:inbox:src", id: pending.ID, wantErr: ctxutil.ErrNoActor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := store.Discard(userCtx(t, tt.user), tt.id, ""); !errors.Is(err, tt.wantErr) {
				t.Errorf("Discard() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
	if got := mustGet(t, store, "src", "pending"); got.Status != inbox.StatusPending {
		t.Errorf("pending message after rejected Discard: status %s, want pending", got.Status)
	}
}

func TestCleanupDeletesOnlySettledMessagesBeforeCutoff(t *testing.T) {
	t.Parallel()

	clk := newClock()
	store, _ := newStore(t, inbox.WithClock(clk.Now))
	cutoff := settleOneOfEach(t, store, clk)

	deleted, err := store.Cleanup(t.Context(), cutoff)
	if err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if deleted != 3 {
		t.Errorf("Cleanup() deleted = %d, want 3", deleted)
	}
	assertRemaining(t, store, []string{"parked-old", "pending-old", "done-new"})
}

// settleOneOfEach stores one message per status, the settled ones settled
// an hour before the returned cutoff, plus one done message settled at the
// cutoff, which is also the clock's time on return.
func settleOneOfEach(t *testing.T, store *inbox.Store, clk *clock) time.Time {
	t.Helper()
	for _, key := range []string{"done-old", "ignored-old", "parked-old", "discarded-old", "pending-old"} {
		mustInsert(t, store, inbox.Message{Source: "src", Key: key, Type: "t"})
	}
	if err := store.Discard(userCtx(t, "alice"), mustGet(t, store, "src", "discarded-old").ID, ""); err != nil {
		t.Fatalf("Discard() error = %v", err)
	}
	w := newWorker(t, store, inbox.Config{
		Source:      "src",
		Concurrency: 5,
		Handle: func(_ context.Context, d inbox.Delivery) error {
			switch d.Key {
			case "ignored-old":
				return inbox.Ignore("unknown type")
			case "parked-old":
				return inbox.Permanent(errors.New("bad signature"))
			case "pending-old":
				return errors.New("provider down")
			}
			return nil
		},
	})
	drain(t, w)
	if got := mustGet(t, store, "src", "pending-old"); got.Status != inbox.StatusPending {
		t.Fatalf("pending-old = %+v, want pending", got)
	}

	clk.Advance(time.Hour)
	mustInsert(t, store, inbox.Message{Source: "src", Key: "done-new", Type: "t"})
	drain(t, w)
	return clk.Now()
}

func assertRemaining(t *testing.T, store *inbox.Store, want []string) {
	t.Helper()
	if keys := recordKeys(listAll(t, store)); !slices.Equal(keys, want) {
		t.Errorf("remaining keys = %v, want %v", keys, want)
	}
}

func recordKeys(rs []inbox.Record) []string {
	var keys []string
	for _, r := range rs {
		keys = append(keys, r.Key)
	}
	return keys
}

// userCtx returns the test context with user as Claims.UserID; an empty
// user leaves the context without an actor.
func userCtx(t *testing.T, user string) context.Context {
	t.Helper()
	if user == "" {
		return t.Context()
	}
	return ctxutil.WithClaims(t.Context(), ctxutil.Claims{UserID: user})
}
