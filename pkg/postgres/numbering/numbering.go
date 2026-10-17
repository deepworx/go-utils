// Package numbering hands out gapless numbers per (tenant, context, name),
// such as invoice numbers. Next runs in the transaction open in the context,
// so a rolled-back transaction gives its numbers back.
//
// PgStore needs the identifier_sequences table from docs/postgres/schema.sql.
// InMemoryStore follows postgres.InMemoryUnitOfWork rollbacks.
package numbering

import (
	"context"
	"errors"

	"github.com/deepworx/go-utils/pkg/ctxutil"
)

var (
	// ErrNoTenant is returned when the context carries no Tenant.
	ErrNoTenant = errors.New("numbering: no tenant in context")
	// ErrEmptySequence is returned when the context or name of a Sequence
	// is empty.
	ErrEmptySequence = errors.New("numbering: empty context or name")
)

// Sequence names a gapless range of numbers within a bounded context. The
// Tenant comes from the context.
type Sequence struct {
	Context string
	Name    string
}

func tenantFor(ctx context.Context, seq Sequence) (string, error) {
	if seq.Context == "" || seq.Name == "" {
		return "", ErrEmptySequence
	}
	tenant, ok := ctxutil.TenantID(ctx)
	if !ok {
		return "", ErrNoTenant
	}
	return tenant, nil
}
