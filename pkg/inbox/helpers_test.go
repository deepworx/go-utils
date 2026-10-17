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

// messageRow is the stored state of one message. Until the operator read
// API exists (DWX-248), tests read it from the table.
type messageRow struct {
	Status   string
	Attempts int
	Error    string
}

func readMessage(t *testing.T, pool *pgxpool.Pool, source, key string) messageRow {
	t.Helper()
	var r messageRow
	err := pool.QueryRow(t.Context(),
		`SELECT status, attempts, COALESCE(error, '') FROM public.inbox_messages WHERE source = $1 AND key = $2`,
		source, key).Scan(&r.Status, &r.Attempts, &r.Error)
	if err != nil {
		t.Fatalf("read message %s/%s: %v", source, key, err)
	}
	return r
}

func countMessages(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.inbox_messages`).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

func mustInsert(t *testing.T, store *inbox.Store, m inbox.Message) {
	t.Helper()
	inserted, err := store.Insert(t.Context(), m)
	if err != nil || !inserted {
		t.Fatalf("Insert(%s/%s) = %v, %v; want true, nil", m.Source, m.Key, inserted, err)
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
