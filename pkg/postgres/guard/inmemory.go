package guard

import (
	"context"
	"fmt"
	"sync"

	"github.com/deepworx/go-utils/pkg/postgres"
)

// InMemoryStore claims keys in memory. Claims are visible at once and
// released when the surrounding postgres.InMemoryUnitOfWork rolls back.
type InMemoryStore struct {
	mu      sync.Mutex
	claimed map[claimKey]struct{}
}

type claimKey struct {
	tenant string
	key    Key
}

// NewInMemoryStore creates an empty InMemoryStore.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{claimed: make(map[claimKey]struct{})}
}

// Claim reserves key for the Tenant in ctx. It returns ErrTaken if the key
// is already claimed, including by another open transaction.
func (s *InMemoryStore) Claim(ctx context.Context, key Key) error {
	tenant, err := tenantFor(ctx, key)
	if err != nil {
		return err
	}
	k := claimKey{tenant: tenant, key: key}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.claimed[k]; taken {
		return fmt.Errorf("claim %s: %w", key.Rule, ErrTaken)
	}
	s.claimed[k] = struct{}{}
	postgres.OnRollback(ctx, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.claimed, k)
	})
	return nil
}
