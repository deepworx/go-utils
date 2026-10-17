package numbering

import (
	"context"
	"sync"

	"github.com/deepworx/go-utils/pkg/postgres"
)

// InMemoryStore hands out numbers from memory. A rolled-back
// postgres.InMemoryUnitOfWork gives its numbers back. Unlike PgStore it does
// not make concurrent transactions wait, so if one rolls back after another
// has drawn a later number from the same sequence, a gap remains.
type InMemoryStore struct {
	mu   sync.Mutex
	last map[sequenceKey]int64
}

type sequenceKey struct {
	tenant string
	seq    Sequence
}

// NewInMemoryStore creates an empty InMemoryStore.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{last: make(map[sequenceKey]int64)}
}

// Next returns the next number of seq for the Tenant in ctx, starting at 1.
func (s *InMemoryStore) Next(ctx context.Context, seq Sequence) (int64, error) {
	tenant, err := tenantFor(ctx, seq)
	if err != nil {
		return 0, err
	}
	k := sequenceKey{tenant: tenant, seq: seq}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.last[k]++
	n := s.last[k]
	postgres.OnRollback(ctx, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.last[k] == n {
			s.last[k]--
		}
	})
	return n, nil
}
