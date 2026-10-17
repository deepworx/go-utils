package postgres

import (
	"context"
	"sync"
)

// InMemoryUnitOfWork implements UnitOfWork without a database. Writes
// registered with Stage inside Execute are applied in order when fn succeeds
// and dropped when it fails or panics, so map-backed prototypes roll back like
// real transactions. Eager writes undo themselves through OnRollback.
type InMemoryUnitOfWork struct{}

// NewInMemoryUnitOfWork creates an in-memory UnitOfWork.
func NewInMemoryUnitOfWork() *InMemoryUnitOfWork {
	return &InMemoryUnitOfWork{}
}

// Execute runs fn in an in-memory transaction and applies its staged writes
// when fn returns nil. If ctx already carries an in-memory transaction, fn
// joins it and its staged writes apply when the outer transaction commits.
func (u *InMemoryUnitOfWork) Execute(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(memTxKey{}).(*memTx); ok {
		return fn(ctx)
	}

	tx := &memTx{}
	committed := false
	defer func() {
		if !committed {
			tx.rollback()
		}
	}()
	if err := fn(context.WithValue(ctx, memTxKey{}, tx)); err != nil {
		return err
	}
	committed = true
	tx.commit()
	return nil
}

// Stage registers apply as an in-memory write. Inside an in-memory
// transaction it is buffered and run in order on commit, or dropped on error.
// Outside one, apply runs immediately.
func Stage(ctx context.Context, apply func()) {
	tx, ok := ctx.Value(memTxKey{}).(*memTx)
	if !ok {
		apply()
		return
	}
	tx.stage(apply)
}

// OnRollback registers undo for an in-memory write that was applied eagerly,
// such as a reservation other transactions must see at once. Inside an
// in-memory transaction undo runs when it fails or panics, in reverse
// registration order, and is dropped on commit. Outside one, undo never runs.
func OnRollback(ctx context.Context, undo func()) {
	tx, ok := ctx.Value(memTxKey{}).(*memTx)
	if !ok {
		return
	}
	tx.onRollback(undo)
}

type memTxKey struct{}

type memTx struct {
	mu     sync.Mutex
	staged []func()
	undos  []func()
}

func (t *memTx) stage(apply func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.staged = append(t.staged, apply)
}

func (t *memTx) onRollback(undo func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.undos = append(t.undos, undo)
}

func (t *memTx) commit() {
	t.mu.Lock()
	staged := t.staged
	t.staged, t.undos = nil, nil
	t.mu.Unlock()

	for _, apply := range staged {
		apply()
	}
}

func (t *memTx) rollback() {
	t.mu.Lock()
	undos := t.undos
	t.staged, t.undos = nil, nil
	t.mu.Unlock()

	for i := len(undos) - 1; i >= 0; i-- {
		undos[i]()
	}
}

var _ UnitOfWork = (*InMemoryUnitOfWork)(nil)
