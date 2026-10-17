// Package guard enforces tenant-scoped uniqueness rules, such as "one user
// per e-mail address". A claim is keyed by (tenant, rule, value) and runs in
// the transaction open in the context, so it commits or rolls back together
// with the rest of the unit of work.
//
// PgStore needs the guards table from docs/postgres/schema.sql.
// InMemoryStore follows postgres.InMemoryUnitOfWork rollbacks.
package guard

import (
	"context"
	"errors"

	"github.com/deepworx/go-utils/pkg/ctxutil"
)

var (
	// ErrTaken is returned when the key is already claimed for the Tenant.
	// The surrounding transaction stays usable.
	ErrTaken = errors.New("guard: key already claimed")
	// ErrNoTenant is returned when the context carries no Tenant.
	ErrNoTenant = errors.New("guard: no tenant in context")
	// ErrEmptyKey is returned when the rule or value of a Key is empty.
	ErrEmptyKey = errors.New("guard: empty rule or value")
)

// Key identifies a unique value within a rule. The Tenant comes from the
// context.
type Key struct {
	Rule  string
	Value string
}

func tenantFor(ctx context.Context, key Key) (string, error) {
	if key.Rule == "" || key.Value == "" {
		return "", ErrEmptyKey
	}
	tenant, ok := ctxutil.TenantID(ctx)
	if !ok {
		return "", ErrNoTenant
	}
	return tenant, nil
}
