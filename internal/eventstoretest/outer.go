package eventstoretest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// outerOneStream: Command transactions joined into one outer transaction
// may only touch one Stream, but may touch it more than once.
func (s suite) outerOneStream(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)
	other := h.openAccount(t, ctx)

	t.Run("two streams", func(t *testing.T) {
		var second error
		err := h.backend.Outer(ctx, func(ctx context.Context) error {
			if _, err := h.accounts.Update(ctx, id, deposit(1)); err != nil {
				return err
			}
			_, second = h.accounts.Update(ctx, other, deposit(1))
			return second
		})
		wantErr(t, second, eventstore.ErrStreamMismatch)
		wantErr(t, err, eventstore.ErrStreamMismatch)
		h.wantVersions(t, map[accountID]int{id: 1, other: 1})
	})

	t.Run("one stream twice", func(t *testing.T) {
		err := h.backend.Outer(ctx, func(ctx context.Context) error {
			for range 2 {
				if _, err := h.accounts.Update(ctx, id, deposit(1)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Outer: %v", err)
		}
		h.wantVersions(t, map[accountID]int{id: 3})
		history, err := h.accounts.History(ctx, id)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		var positions []int
		for _, env := range history {
			positions = append(positions, env.StreamPosition)
		}
		if !slices.Equal(positions, []int{1, 2, 3}) {
			t.Fatalf("stream positions = %v, want [1 2 3]", positions)
		}
	})
}

// outerHoldsStreamLock: a Command transaction joined into an outer
// transaction keeps its Stream locked until the outer transaction ends.
func (s suite) outerHoldsStreamLock(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)

	written := make(chan struct{})
	finish := make(chan struct{})
	outerDone := make(chan error, 1)
	go func() {
		outerDone <- h.backend.Outer(ctx, func(ctx context.Context) error {
			if _, err := h.accounts.Update(ctx, id, deposit(1)); err != nil {
				return err
			}
			close(written)
			<-finish
			return nil
		})
	}()
	select {
	case <-written:
	case err := <-outerDone:
		t.Fatalf("Outer: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, err := h.accounts.Update(waitCtx, id, deposit(1))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Update while the outer transaction is open = %v, want it to wait for the stream lock", err)
	}

	close(finish)
	if err := <-outerDone; err != nil {
		t.Fatalf("Outer: %v", err)
	}
	if _, err := h.accounts.Update(ctx, id, deposit(1)); err != nil {
		t.Fatalf("Update after the outer commit: %v", err)
	}
	h.wantVersions(t, map[accountID]int{id: 3})
}

func (h harness) wantVersions(t *testing.T, want map[accountID]int) {
	t.Helper()
	for id, version := range want {
		got, err := h.accounts.Load(userCtx("t1"), id)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Version() != version {
			t.Errorf("version of %v = %d, want %d", id, got.Version(), version)
		}
	}
}
