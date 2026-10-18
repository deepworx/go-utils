package eventstoretest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

func newCommandID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

// counted wraps fn and counts its runs.
func counted[A any](runs *atomic.Int32, fn func(context.Context, A) error) func(context.Context, A) error {
	return func(ctx context.Context, agg A) error {
		runs.Add(1)
		return fn(ctx, agg)
	}
}

func wantReplay(t *testing.T, got, original eventstore.CommandResult) {
	t.Helper()
	want := original
	want.Replayed = true
	if original.Replayed || got != want {
		t.Fatalf("repeat = %+v, want the original %+v replayed", got, original)
	}
}

type accountState struct {
	version int
	balance int
}

func (h harness) wantAccount(t *testing.T, id accountID, want accountState) {
	t.Helper()
	got, err := h.accounts.Load(userCtx("t1"), id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if (accountState{version: got.Version(), balance: got.balance}) != want {
		t.Fatalf("version %d balance %d, want %+v", got.Version(), got.balance, want)
	}
}

func (s suite) commandIDRepeatedUpdate(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)
	cmd := eventstore.WithCommandID(newCommandID())
	var runs atomic.Int32

	first, err := h.accounts.Update(ctx, id, counted(&runs, deposit(1)), cmd)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	repeat, err := h.accounts.Update(ctx, id, counted(&runs, deposit(1)), cmd)
	if err != nil {
		t.Fatalf("repeated Update: %v", err)
	}
	wantReplay(t, repeat, first)
	if n := runs.Load(); n != 1 {
		t.Fatalf("fn ran %d times, want 1", n)
	}
	h.wantAccount(t, id, accountState{version: 2, balance: 1})
}

func (s suite) commandIDRepeatedCreate(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := newAccountID()
	cmd := eventstore.WithCommandID(newCommandID())
	var runs atomic.Int32
	open := counted(&runs, func(_ context.Context, a *account) error {
		a.open("alice")
		return nil
	})

	first, err := h.accounts.Create(ctx, id, open, cmd)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	repeat, err := h.accounts.Create(ctx, id, open, cmd)
	if err != nil {
		t.Fatalf("repeated Create: %v", err)
	}
	wantReplay(t, repeat, first)
	if n := runs.Load(); n != 1 {
		t.Fatalf("fn ran %d times, want 1", n)
	}
	h.wantAccount(t, id, accountState{version: 1, balance: 0})
}

func (s suite) commandIDReused(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)
	other := h.openAccount(t, ctx)
	cmdID := newCommandID()
	cmd := eventstore.WithCommandID(cmdID)
	if _, err := h.accounts.Update(ctx, id, deposit(1), cmd); err != nil {
		t.Fatalf("Update: %v", err)
	}

	tests := []struct {
		name string
		run  func() error
	}{
		{"update of another stream", func() error {
			_, err := h.accounts.Update(ctx, other, deposit(1), cmd)
			return err
		}},
		{"create of another stream", func() error {
			_, err := h.accounts.Create(ctx, newAccountID(), deposit(1), cmd)
			return err
		}},
		{"create of another stream type", func() error {
			_, err := h.orders.Create(ctx, orderID(newAccountID()), func(_ context.Context, o *order) error {
				o.place()
				return nil
			}, cmd)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantErr(t, tt.run(), eventstore.ErrCommandIDReused)
		})
	}
	h.wantAccount(t, other, accountState{version: 1, balance: 0})

	t.Run("same id in another tenant", func(t *testing.T) {
		t2 := userCtx("t2")
		res, err := h.accounts.Create(t2, other, func(_ context.Context, a *account) error {
			a.open("bob")
			return nil
		}, cmd)
		if err != nil || res.Replayed {
			t.Fatalf("Create in t2 = %+v, %v, want a fresh command", res, err)
		}
	})
}

func (s suite) commandIDNoOpRecorded(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)
	cmd := eventstore.WithCommandID(newCommandID())
	var runs atomic.Int32
	noop := counted(&runs, func(context.Context, *account) error { return nil })

	first, err := h.accounts.Update(ctx, id, noop, cmd)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if first != (eventstore.CommandResult{Version: 1}) {
		t.Fatalf("no-op result = %+v, want zero Position and version 1", first)
	}
	if _, err := h.accounts.Update(ctx, id, deposit(1)); err != nil {
		t.Fatalf("Update without command id: %v", err)
	}
	repeat, err := h.accounts.Update(ctx, id, noop, cmd)
	if err != nil {
		t.Fatalf("repeated Update: %v", err)
	}
	wantReplay(t, repeat, first)
	if n := runs.Load(); n != 1 {
		t.Fatalf("fn ran %d times, want 1", n)
	}
}

func (s suite) commandIDRejectedLeavesNoRecord(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)
	cmd := eventstore.WithCommandID(newCommandID())

	_, err := h.accounts.Update(ctx, id, func(_ context.Context, a *account) error {
		a.deposit(1)
		return errRejected
	}, cmd)
	wantErr(t, err, errRejected)

	var runs atomic.Int32
	retry, err := h.accounts.Update(ctx, id, counted(&runs, deposit(2)), cmd)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retry.Replayed || runs.Load() != 1 || retry.Version != 2 {
		t.Fatalf("retry = %+v after %d runs, want a fresh command at version 2", retry, runs.Load())
	}
	h.wantAccount(t, id, accountState{version: 2, balance: 2})
}

func (s suite) commandIDBeforeExpectVersion(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)
	opts := []eventstore.CommandOption{eventstore.ExpectVersion(1), eventstore.WithCommandID(newCommandID())}

	first, err := h.accounts.Update(ctx, id, deposit(1), opts...)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := h.accounts.Update(ctx, id, deposit(1)); err != nil {
		t.Fatalf("Update without command id: %v", err)
	}
	repeat, err := h.accounts.Update(ctx, id, deposit(1), opts...)
	if err != nil {
		t.Fatalf("repeat with a stale expected version: %v", err)
	}
	wantReplay(t, repeat, first)
	h.wantAccount(t, id, accountState{version: 3, balance: 2})
}

func (s suite) commandIDConcurrent(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)
	cmd := eventstore.WithCommandID(newCommandID())

	const writers = 20
	var (
		wg      sync.WaitGroup
		runs    atomic.Int32
		results = make([]eventstore.CommandResult, writers)
		errs    = make([]error, writers)
	)
	for i := range writers {
		wg.Go(func() {
			results[i], errs[i] = h.accounts.Update(ctx, id, counted(&runs, deposit(1)), cmd)
		})
	}
	wg.Wait()

	var original eventstore.CommandResult
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("Update: %v", errs[i])
		}
		if !res.Replayed {
			original = res
		}
	}
	for _, res := range results {
		if res.Replayed {
			wantReplay(t, res, original)
		}
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("fn ran %d times, want 1", n)
	}
	h.wantAccount(t, id, accountState{version: 2, balance: 1})
}

func (s suite) commandIDInOneTransaction(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)
	uow := h.backend.UnitOfWork

	t.Run("a repeat sees the pending record", func(t *testing.T) {
		cmd := eventstore.WithCommandID(newCommandID())
		err := uow.Execute(ctx, uuid.UUID(id), func(ctx context.Context) error {
			first, err := h.accounts.Update(ctx, id, deposit(1), cmd)
			if err != nil {
				return err
			}
			repeat, err := h.accounts.Update(ctx, id, deposit(1), cmd)
			if err != nil {
				return err
			}
			wantReplay(t, repeat, first)
			return nil
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		h.wantAccount(t, id, accountState{version: 2, balance: 1})
	})

	t.Run("a rollback drops the record", func(t *testing.T) {
		cmd := eventstore.WithCommandID(newCommandID())
		err := uow.Execute(ctx, uuid.UUID(id), func(ctx context.Context) error {
			if _, err := h.accounts.Update(ctx, id, deposit(1), cmd); err != nil {
				return err
			}
			return errRejected
		})
		wantErr(t, err, errRejected)
		retry, err := h.accounts.Update(ctx, id, deposit(1), cmd)
		if err != nil || retry.Replayed {
			t.Fatalf("retry = %+v, %v, want a fresh command", retry, err)
		}
		h.wantAccount(t, id, accountState{version: 3, balance: 2})
	})

	t.Run("a nested repeat conflicts", func(t *testing.T) {
		cmd := eventstore.WithCommandID(newCommandID())
		err := uow.Execute(ctx, uuid.UUID(id), func(ctx context.Context) error {
			_, err := h.accounts.Update(ctx, id, func(ctx context.Context, _ *account) error {
				_, err := h.accounts.Update(ctx, id, deposit(1), cmd)
				return err
			}, cmd)
			return err
		})
		wantErr(t, err, eventstore.ErrConcurrencyConflict)
		h.wantAccount(t, id, accountState{version: 3, balance: 2})
	})
}

func (s suite) commandIDConcurrentOnOtherStream(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)
	other := h.openAccount(t, ctx)
	cmd := eventstore.WithCommandID(newCommandID())

	recorded := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- h.backend.UnitOfWork.Execute(ctx, uuid.UUID(id), func(ctx context.Context) error {
			if _, err := h.accounts.Update(ctx, id, deposit(1), cmd); err != nil {
				return err
			}
			close(recorded)
			<-release
			return nil
		})
	}()
	select {
	case <-recorded:
	case err := <-first:
		t.Fatalf("first command: %v", err)
	}

	second := make(chan error, 1)
	go func() {
		_, err := h.accounts.Update(ctx, other, deposit(1), cmd)
		second <- err
	}()
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first command: %v", err)
	}
	err := <-second
	if !errors.Is(err, eventstore.ErrConcurrencyConflict) && !errors.Is(err, eventstore.ErrCommandIDReused) {
		t.Fatalf("second command: %v, want ErrConcurrencyConflict or ErrCommandIDReused", err)
	}
	h.wantAccount(t, id, accountState{version: 2, balance: 1})
	h.wantAccount(t, other, accountState{version: 1, balance: 0})
}
