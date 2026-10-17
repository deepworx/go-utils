// Package eventstoretest is the write-side contract suite that every
// eventstore backend (Pg and in-memory) must pass. A backend test calls
//
//	eventstoretest.Run(t, func(t *testing.T) eventstoretest.Backend { ... })
//
// and the suite exercises it only through eventstore.Repository and
// eventstore.UnitOfWork.
package eventstoretest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/postgres/guard"
	"github.com/deepworx/go-utils/pkg/postgres/numbering"
)

// StoreConfig configures one Store opened by a Backend.
type StoreConfig struct {
	Codec *eventstore.Codec
	// Clock is passed to eventstore.WithClock.
	Clock func() time.Time
}

// Guard is the claim surface of guard.PgStore and guard.InMemoryStore.
type Guard interface {
	Claim(ctx context.Context, key guard.Key) error
}

// Numbering is the surface of numbering.PgStore and numbering.InMemoryStore.
type Numbering interface {
	Next(ctx context.Context, seq numbering.Sequence) (int64, error)
}

// Backend is one empty event log under test.
type Backend struct {
	// Store opens a Store over the Backend's log. Stores opened from one
	// Backend share their events, whatever their Codec.
	Store      func(cfg StoreConfig) eventstore.Store
	UnitOfWork eventstore.UnitOfWork
	// Guard and Numbering must join the UnitOfWork's transactions.
	Guard     Guard
	Numbering Numbering
}

// Run runs the contract suite. newBackend is called once per subtest.
func Run(t *testing.T, newBackend func(t *testing.T) Backend) {
	t.Helper()
	s := suite{newBackend: newBackend}
	tests := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{"CreateThenLoad", s.createThenLoad},
		{"CreateExisting", s.createExisting},
		{"NotFound", s.notFound},
		{"ExpectedVersion", s.expectedVersion},
		{"IdempotentRepeat", s.idempotentRepeat},
		{"LoadAtAndHistory", s.loadAtAndHistory},
		{"ConcurrentUpdatesSerialize", s.concurrentUpdatesSerialize},
		{"TenantIsolation", s.tenantIsolation},
		{"NoTenant", s.noTenant},
		{"Actor", s.actor},
		{"Correlation", s.correlation},
		{"Causation", s.causation},
		{"RecordedAtFromClock", s.recordedAtFromClock},
		{"Upcasting", s.upcasting},
		{"CommandTransactionRules", s.commandTransactionRules},
		{"WithinJoins", s.withinJoins},
		{"FailedCommandRollsBack", s.failedCommandRollsBack},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.fn(t)
		})
	}
}

type suite struct {
	newBackend func(t *testing.T) Backend
}

var epoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type harness struct {
	backend  Backend
	store    eventstore.Store
	accounts *eventstore.Repository[accountID, account, *account]
	orders   *eventstore.Repository[orderID, order, *order]
}

func (s suite) setup(t *testing.T) harness {
	t.Helper()
	b := s.newBackend(t)
	store := b.Store(StoreConfig{Codec: newCodec(), Clock: func() time.Time { return epoch }})
	cfg := eventstore.RepositoryConfig{Store: store, UnitOfWork: b.UnitOfWork}
	return harness{
		backend:  b,
		store:    store,
		accounts: eventstore.NewRepository[accountID, account](cfg),
		orders:   eventstore.NewRepository[orderID, order](cfg),
	}
}

func userCtx(tenant string) context.Context {
	ctx := ctxutil.WithTenantID(context.Background(), tenant)
	return ctxutil.WithClaims(ctx, ctxutil.Claims{UserID: "user-1"})
}

func newAccountID() accountID { return accountID(uuid.Must(uuid.NewV7())) }

func (s suite) createThenLoad(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := newAccountID()

	res, err := h.accounts.Create(ctx, id, func(_ context.Context, a *account) error {
		a.open("alice")
		a.deposit(5)
		return nil
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Version != 2 || res.Position == (eventstore.Cursor{}) {
		t.Fatalf("Create result = %+v, want version 2 and a position", res)
	}

	got, err := h.accounts.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ID() != id || got.Version() != 2 || got.owner != "alice" || got.balance != 5 {
		t.Fatalf("Load = id %v version %d owner %q balance %d, want %v 2 alice 5",
			got.ID(), got.Version(), got.owner, got.balance, id)
	}
}
