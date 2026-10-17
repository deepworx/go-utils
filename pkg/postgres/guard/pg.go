package guard

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/pkg/postgres"
)

// PgOption configures a PgStore.
type PgOption func(*PgStore)

// WithSchema sets the Postgres schema holding the guards table. Defaults to
// public.
func WithSchema(schema string) PgOption {
	return func(s *PgStore) { s.schema = schema }
}

// PgStore claims keys in the guards table.
type PgStore struct {
	pool   *pgxpool.Pool
	schema string
	claim  string
}

// NewPgStore creates a PgStore that runs on the transaction in the context,
// or on pool outside one.
func NewPgStore(pool *pgxpool.Pool, opts ...PgOption) *PgStore {
	s := &PgStore{pool: pool, schema: "public"}
	for _, opt := range opts {
		opt(s)
	}
	table := pgx.Identifier{s.schema, "guards"}.Sanitize()
	s.claim = `INSERT INTO ` + table + ` (tenant_id, rule, key) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, rule, key) DO NOTHING`
	return s
}

// Claim reserves key for the Tenant in ctx. It returns ErrTaken if the key
// is already claimed. A claim made by another open transaction blocks until
// that transaction ends. Rolling back the surrounding transaction releases
// the claim.
func (s *PgStore) Claim(ctx context.Context, key Key) error {
	tenant, err := tenantFor(ctx, key)
	if err != nil {
		return err
	}
	tag, err := postgres.Conn(ctx, s.pool).Exec(ctx, s.claim, tenant, key.Rule, key.Value)
	if err != nil {
		return fmt.Errorf("guard: claim %s: %w", key.Rule, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("claim %s: %w", key.Rule, ErrTaken)
	}
	return nil
}
