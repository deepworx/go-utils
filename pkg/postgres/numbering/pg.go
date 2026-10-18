package numbering

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/pkg/postgres"
)

// PgOption configures a PgStore.
type PgOption func(*PgStore)

// WithSchema sets the Postgres schema holding the identifier_sequences
// table. Defaults to public.
func WithSchema(schema string) PgOption {
	return func(s *PgStore) { s.schema = schema }
}

// PgStore hands out numbers from the identifier_sequences table.
type PgStore struct {
	pool   *pgxpool.Pool
	schema string
	next   string
}

// NewPgStore creates a PgStore that runs on the transaction in the context,
// or on pool outside one.
func NewPgStore(pool *pgxpool.Pool, opts ...PgOption) *PgStore {
	s := &PgStore{pool: pool, schema: "public"}
	for _, opt := range opts {
		opt(s)
	}
	table := pgx.Identifier{s.schema, "identifier_sequences"}.Sanitize()
	s.next = `INSERT INTO ` + table + ` AS s (tenant_id, context, name, last_value) VALUES ($1, $2, $3, 1)
		ON CONFLICT (tenant_id, context, name) DO UPDATE SET last_value = s.last_value + 1
		RETURNING last_value`
	return s
}

// Next returns the next number of seq for the Tenant in ctx, starting at 1.
// The sequence row stays locked until the surrounding transaction ends, so
// concurrent callers wait and a rollback leaves no gap. Outside a
// transaction the number is used up at once.
func (s *PgStore) Next(ctx context.Context, seq Sequence) (int64, error) {
	tenant, err := tenantFor(ctx, seq)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := postgres.Conn(ctx, s.pool).QueryRow(ctx, s.next, tenant, seq.Context, seq.Name).Scan(&n); err != nil {
		return 0, fmt.Errorf("numbering: next %s/%s: %w", seq.Context, seq.Name, err)
	}
	return n, nil
}
