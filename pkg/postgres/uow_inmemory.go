package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// InMemoryUnitOfWork implements UnitOfWork as a no-op for testing.
// The callback receives a nilTransaction where Tx() returns nil.
// Use with components that handle nil transactions (e.g., InMemoryEventStore).
type InMemoryUnitOfWork struct{}

// NewInMemoryUnitOfWork creates a no-op UnitOfWork for unit testing.
func NewInMemoryUnitOfWork() *InMemoryUnitOfWork {
	return &InMemoryUnitOfWork{}
}

// Execute runs fn without transaction management.
func (u *InMemoryUnitOfWork) Execute(ctx context.Context, fn func(ctx context.Context, tx Transaction) error) error {
	return fn(ctx, nilTransaction{})
}

// nilTransaction implements Transaction with Tx() returning nil.
type nilTransaction struct{}

func (nilTransaction) Tx() pgx.Tx {
	return nil
}

// compile-time checks
var (
	_ UnitOfWork  = (*InMemoryUnitOfWork)(nil)
	_ Transaction = nilTransaction{}
)
