package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Transaction represents an active database transaction.
// For in-memory implementations, Tx() returns nil.
type Transaction interface {
	Tx() pgx.Tx
}

// UnitOfWork manages transaction boundaries.
type UnitOfWork interface {
	Execute(ctx context.Context, fn func(ctx context.Context, tx Transaction) error) error
}

// PgUnitOfWork implements UnitOfWork using a PostgreSQL connection pool.
type PgUnitOfWork struct {
	pool *pgxpool.Pool
}

// NewUnitOfWork creates a UnitOfWork backed by the given connection pool.
func NewUnitOfWork(pool *pgxpool.Pool) *PgUnitOfWork {
	return &PgUnitOfWork{pool: pool}
}

// Execute runs fn within a transaction.
// Commits on success, rolls back on error or panic.
func (u *PgUnitOfWork) Execute(ctx context.Context, fn func(ctx context.Context, tx Transaction) error) error {
	return WithTx(ctx, u.pool, func(tx pgx.Tx) error {
		return fn(ctx, &pgTransaction{tx: tx})
	})
}

// pgTransaction wraps pgx.Tx to implement Transaction.
type pgTransaction struct {
	tx pgx.Tx
}

func (t *pgTransaction) Tx() pgx.Tx {
	return t.tx
}

// compile-time checks
var (
	_ UnitOfWork  = (*PgUnitOfWork)(nil)
	_ Transaction = (*pgTransaction)(nil)
)
