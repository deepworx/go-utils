package inbox_test

import (
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/inbox"
)

func newStore(t *testing.T, opts ...inbox.StoreOption) (*inbox.Store, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.New(t)
	store, err := inbox.NewStore(pool, inbox.StoreConfig{}, opts...)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return store, pool
}

// listAll returns every stored message.
func listAll(t *testing.T, store *inbox.Store) []inbox.Record {
	t.Helper()
	rs, err := store.List(t.Context(), inbox.Filter{Limit: 1000})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	return rs
}

// mustGet returns the stored message source/key.
func mustGet(t *testing.T, store *inbox.Store, source, key string) inbox.Record {
	t.Helper()
	for _, r := range listAll(t, store) {
		if r.Source == source && r.Key == key {
			return r
		}
	}
	t.Fatalf("message %s/%s not listed", source, key)
	return inbox.Record{}
}

func mustInsert(t *testing.T, store *inbox.Store, m inbox.Message) {
	t.Helper()
	inserted, err := store.Insert(t.Context(), m)
	if err != nil || !inserted {
		t.Fatalf("Insert(%s/%s, tenant %q) = %v, %v; want true, nil", m.Source, m.Key, m.Tenant, inserted, err)
	}
}

// clock is a controllable time source for inbox.WithClock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Now().UTC().Truncate(time.Microsecond)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
