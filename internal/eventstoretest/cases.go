package eventstoretest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/postgres/guard"
	"github.com/deepworx/go-utils/pkg/postgres/numbering"
)

var errRejected = errors.New("rejected by command")

func (h harness) openAccount(t *testing.T, ctx context.Context) accountID {
	t.Helper()
	id := newAccountID()
	if _, err := h.accounts.Create(ctx, id, func(_ context.Context, a *account) error {
		a.open("alice")
		return nil
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

func deposit(amount int) func(context.Context, *account) error {
	return func(_ context.Context, a *account) error {
		a.deposit(amount)
		return nil
	}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func (s suite) createExisting(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)

	_, err := h.accounts.Create(ctx, id, deposit(1))
	wantErr(t, err, eventstore.ErrAlreadyExists)

	_, err = h.orders.Create(ctx, orderID(id), func(_ context.Context, o *order) error {
		o.place()
		return nil
	})
	wantErr(t, err, eventstore.ErrAlreadyExists)
}

func (s suite) notFound(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	existing := h.openAccount(t, ctx)

	tests := []struct {
		name string
		id   orderID
	}{
		{"missing stream", orderID(newAccountID())},
		{"other stream type", orderID(existing)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.orders.Load(ctx, tt.id)
			wantErr(t, err, eventstore.ErrNotFound)
			_, err = h.orders.LoadAt(ctx, tt.id, 1)
			wantErr(t, err, eventstore.ErrNotFound)
			_, err = h.orders.History(ctx, tt.id)
			wantErr(t, err, eventstore.ErrNotFound)
			_, err = h.orders.Update(ctx, tt.id, func(context.Context, *order) error { return nil })
			wantErr(t, err, eventstore.ErrNotFound)
		})
	}
}

func (s suite) expectedVersion(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)

	tests := []struct {
		name    string
		expect  []eventstore.CommandOption
		wantErr error
		version int
	}{
		{"matching", []eventstore.CommandOption{eventstore.ExpectVersion(1)}, nil, 2},
		{"stale", []eventstore.CommandOption{eventstore.ExpectVersion(1)}, eventstore.ErrConcurrencyConflict, 2},
		{"zero disables the check", []eventstore.CommandOption{eventstore.ExpectVersion(0)}, nil, 3},
		{"omitted", nil, nil, 4},
	}
	for _, tt := range tests {
		res, err := h.accounts.Update(ctx, id, deposit(1), tt.expect...)
		if tt.wantErr != nil {
			wantErr(t, err, tt.wantErr)
		} else if err != nil {
			t.Fatalf("%s: Update: %v", tt.name, err)
		}
		got, err := h.accounts.Load(ctx, id)
		if err != nil {
			t.Fatalf("%s: Load: %v", tt.name, err)
		}
		if got.Version() != tt.version || (err == nil && tt.wantErr == nil && res.Version != tt.version) {
			t.Fatalf("%s: version = %d (result %d), want %d", tt.name, got.Version(), res.Version, tt.version)
		}
	}
}

func (s suite) idempotentRepeat(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)

	res, err := h.accounts.Update(ctx, id, func(context.Context, *account) error { return nil })
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if res != (eventstore.CommandResult{Version: 1}) {
		t.Fatalf("result = %+v, want zero Position and version 1", res)
	}
}

func (s suite) loadAtAndHistory(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)
	first, err := h.accounts.Update(ctx, id, deposit(10))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	second, err := h.accounts.Update(ctx, id, deposit(20))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if second.Position.Compare(first.Position) <= 0 {
		t.Fatalf("positions %+v then %+v, want increasing", first.Position, second.Position)
	}

	at2, err := h.accounts.LoadAt(ctx, id, 2)
	if err != nil {
		t.Fatalf("LoadAt: %v", err)
	}
	if at2.Version() != 2 || at2.balance != 10 {
		t.Fatalf("LoadAt(2) = version %d balance %d, want 2 and 10", at2.Version(), at2.balance)
	}
	_, err = h.accounts.LoadAt(ctx, id, 4)
	wantErr(t, err, eventstore.ErrNotFound)
	_, err = h.accounts.LoadAt(ctx, id, 0)
	wantErr(t, err, eventstore.ErrInvalidVersion)

	history, err := h.accounts.History(ctx, id)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	want := []struct {
		eventType string
		event     any
	}{
		{"Test.Account.Opened", opened{Owner: "alice"}},
		{"Test.Account.Deposited", deposited{Amount: 10}},
		{"Test.Account.Deposited", deposited{Amount: 20}},
	}
	if len(history) != len(want) {
		t.Fatalf("History has %d events, want %d", len(history), len(want))
	}
	eventIDs := map[uuid.UUID]bool{}
	for i, env := range history {
		if env.EventType != want[i].eventType || env.Event != want[i].event || env.StreamPosition != i+1 ||
			env.StreamID != uuid.UUID(id) || env.StreamType != "Test.Account" || env.TenantID != "t1" {
			t.Fatalf("History[%d] = %+v, want %s %+v at position %d", i, env, want[i].eventType, want[i].event, i+1)
		}
		if env.EventID == uuid.Nil || eventIDs[env.EventID] {
			t.Fatalf("History[%d] EventID %v is nil or repeated", i, env.EventID)
		}
		eventIDs[env.EventID] = true
	}
	if history[1].Cursor != first.Position || history[2].Cursor != second.Position {
		t.Fatalf("cursors %+v %+v, want the command positions %+v %+v", history[1].Cursor, history[2].Cursor, first.Position, second.Position)
	}
}

func (s suite) concurrentUpdatesSerialize(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Go(func() {
			_, err := h.accounts.Update(ctx, id, deposit(1))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	got, err := h.accounts.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.balance != writers || got.Version() != writers+1 {
		t.Fatalf("balance %d version %d, want %d and %d", got.balance, got.Version(), writers, writers+1)
	}
}

func (s suite) tenantIsolation(t *testing.T) {
	h := s.setup(t)
	id := newAccountID()
	for _, tenant := range []string{"t1", "t2"} {
		if _, err := h.accounts.Create(userCtx(tenant), id, func(_ context.Context, a *account) error {
			a.open(tenant)
			return nil
		}); err != nil {
			t.Fatalf("Create in %s: %v", tenant, err)
		}
	}
	if _, err := h.accounts.Update(userCtx("t2"), id, deposit(7), eventstore.ExpectVersion(1)); err != nil {
		t.Fatalf("Update in t2: %v", err)
	}

	tests := []struct {
		tenant  string
		version int
		balance int
	}{
		{"t1", 1, 0},
		{"t2", 2, 7},
	}
	for _, tt := range tests {
		got, err := h.accounts.Load(userCtx(tt.tenant), id)
		if err != nil {
			t.Fatalf("Load in %s: %v", tt.tenant, err)
		}
		if got.owner != tt.tenant || got.Version() != tt.version || got.balance != tt.balance {
			t.Fatalf("%s: owner %q version %d balance %d, want %q %d %d",
				tt.tenant, got.owner, got.Version(), got.balance, tt.tenant, tt.version, tt.balance)
		}
	}
	_, err := h.accounts.Load(userCtx("t3"), id)
	wantErr(t, err, eventstore.ErrNotFound)
}

func (s suite) noTenant(t *testing.T) {
	h := s.setup(t)
	ctx := ctxutil.WithActor(context.Background(), "user-1")
	id := newAccountID()

	_, err := h.accounts.Create(ctx, id, deposit(1))
	wantErr(t, err, eventstore.ErrNoTenant)
	_, err = h.accounts.Update(ctx, id, deposit(1))
	wantErr(t, err, eventstore.ErrNoTenant)
	_, err = h.accounts.Load(ctx, id)
	wantErr(t, err, eventstore.ErrNoTenant)
	_, err = h.accounts.History(ctx, id)
	wantErr(t, err, eventstore.ErrNoTenant)
	err = h.backend.UnitOfWork.Execute(ctx, uuid.UUID(id), func(context.Context) error { return nil })
	wantErr(t, err, eventstore.ErrNoTenant)
}

// metadataOf creates an account under ctx and returns its first Envelope.
func (h harness) metadataOf(t *testing.T, ctx context.Context) (eventstore.Envelope, error) {
	t.Helper()
	id := newAccountID()
	if _, err := h.accounts.Create(ctx, id, func(_ context.Context, a *account) error {
		a.open("alice")
		return nil
	}); err != nil {
		return eventstore.Envelope{}, err
	}
	history, err := h.accounts.History(ctxutil.WithTenantID(context.Background(), "t1"), id)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	return history[0], nil
}

func (s suite) actor(t *testing.T) {
	h := s.setup(t)
	base := ctxutil.WithTenantID(context.Background(), "t1")
	user := func(id string) func(context.Context) context.Context {
		return func(ctx context.Context) context.Context {
			return ctxutil.WithClaims(ctx, ctxutil.Claims{UserID: id})
		}
	}
	actorAndUser := func(actor, id string) func(context.Context) context.Context {
		return func(ctx context.Context) context.Context {
			return user(id)(ctxutil.WithActor(ctx, actor))
		}
	}

	tests := []struct {
		name    string
		with    func(context.Context) context.Context
		want    string
		wantErr error
	}{
		{"explicit actor wins over claims", actorAndUser("system:reactor:mailer", "user-1"), "system:reactor:mailer", nil},
		{"claims user", user("user-1"), "user-1", nil},
		{"empty actor falls back to claims", actorAndUser("", "user-1"), "user-1", nil},
		{"none", func(ctx context.Context) context.Context { return ctx }, "", eventstore.ErrNoActor},
		{"empty claims user", user(""), "", eventstore.ErrNoActor},
		{"reserved prefix in claims", user("system:scheduler:x"), "", eventstore.ErrNoActor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := h.metadataOf(t, tt.with(base))
			if tt.wantErr != nil {
				wantErr(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if env.Actor != tt.want {
				t.Fatalf("Actor = %q, want %q", env.Actor, tt.want)
			}
		})
	}
}

func (s suite) correlation(t *testing.T) {
	h := s.setup(t)
	base := userCtx("t1")

	tests := []struct {
		name string
		with func(context.Context) context.Context
		want string
	}{
		{"explicit correlation wins over request ID", func(ctx context.Context) context.Context {
			return ctxutil.WithRequestID(ctxutil.WithCorrelationID(ctx, "corr-1"), "req-1")
		}, "corr-1"},
		{"request ID", func(ctx context.Context) context.Context {
			return ctxutil.WithRequestID(ctx, "req-1")
		}, "req-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := h.metadataOf(t, tt.with(base))
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if env.CorrelationID != tt.want {
				t.Fatalf("CorrelationID = %q, want %q", env.CorrelationID, tt.want)
			}
		})
	}

	for _, requestID := range []string{strings.Repeat("r", 129), "req\n1", "req 1", "räq"} {
		t.Run(fmt.Sprintf("invalid request ID %.12q is not stored", requestID), func(t *testing.T) {
			env, err := h.metadataOf(t, ctxutil.WithRequestID(base, requestID))
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if generated, err := uuid.Parse(env.CorrelationID); err != nil || generated.Version() != 7 {
				t.Fatalf("CorrelationID = %q, want a generated UUIDv7", env.CorrelationID)
			}
		})
	}

	t.Run("generated per command transaction", func(t *testing.T) {
		id := newAccountID()
		if _, err := h.accounts.Create(base, id, func(_ context.Context, a *account) error {
			a.open("alice")
			a.deposit(1)
			return nil
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := h.accounts.Update(base, id, deposit(1)); err != nil {
			t.Fatalf("Update: %v", err)
		}
		history, err := h.accounts.History(base, id)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		generated, err := uuid.Parse(history[0].CorrelationID)
		if err != nil || generated.Version() != 7 {
			t.Fatalf("CorrelationID %q is not a UUIDv7", history[0].CorrelationID)
		}
		if history[1].CorrelationID != history[0].CorrelationID {
			t.Fatalf("one command got correlations %q and %q", history[0].CorrelationID, history[1].CorrelationID)
		}
		if history[2].CorrelationID == history[0].CorrelationID {
			t.Fatalf("two commands share correlation %q", history[0].CorrelationID)
		}
	})
}

func (s suite) causation(t *testing.T) {
	h := s.setup(t)
	cause := uuid.Must(uuid.NewV7())

	tests := []struct {
		name string
		with func(context.Context) context.Context
		want uuid.UUID
	}{
		{"set", func(ctx context.Context) context.Context { return ctxutil.WithCausationID(ctx, cause) }, cause},
		{"absent", func(ctx context.Context) context.Context { return ctx }, uuid.Nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := h.metadataOf(t, tt.with(userCtx("t1")))
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if env.CausationID != tt.want {
				t.Fatalf("CausationID = %v, want %v", env.CausationID, tt.want)
			}
		})
	}
}

func (s suite) recordedAtFromClock(t *testing.T) {
	h := s.setup(t)
	env, err := h.metadataOf(t, userCtx("t1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !env.RecordedAt.Equal(epoch) {
		t.Fatalf("RecordedAt = %v, want %v", env.RecordedAt, epoch)
	}
}

func (s suite) upcasting(t *testing.T) {
	b := s.newBackend(t)
	clock := func() time.Time { return epoch }
	v1 := eventstore.NewRepository[accountID, account](eventstore.RepositoryConfig{
		Store:      b.Store(StoreConfig{Codec: newCodecV1(), Clock: clock}),
		UnitOfWork: b.UnitOfWork,
	})
	current := eventstore.NewRepository[accountID, account](eventstore.RepositoryConfig{
		Store:      b.Store(StoreConfig{Codec: newCodec(), Clock: clock}),
		UnitOfWork: b.UnitOfWork,
	})
	ctx := userCtx("t1")
	id := newAccountID()

	if _, err := v1.Create(ctx, id, func(_ context.Context, a *account) error {
		a.open("alice")
		raiseV1Label(a, "vip")
		return nil
	}); err != nil {
		t.Fatalf("Create with v1 codec: %v", err)
	}

	got, err := current.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load with current codec: %v", err)
	}
	if got.label != "vip" {
		t.Fatalf("label = %q, want the upcast v1 name %q", got.label, "vip")
	}
	history, err := current.History(ctx, id)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if history[1].Event != (labelled{Label: "vip"}) {
		t.Fatalf("History[1].Event = %#v, want the upcast labelled", history[1].Event)
	}
}

func (s suite) commandTransactionRules(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)
	other := h.openAccount(t, ctx)
	uow := h.backend.UnitOfWork

	t.Run("save outside a command transaction", func(t *testing.T) {
		agg, err := h.accounts.Load(ctx, id)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		agg.deposit(1)
		_, err = h.accounts.Save(ctx, agg)
		wantErr(t, err, eventstore.ErrNotCommandTransaction)
	})

	tests := []struct {
		name string
		fn   func(ctx context.Context) error
		want error
	}{
		{"save of another stream", func(ctx context.Context) error {
			agg, err := h.accounts.Load(ctx, other)
			if err != nil {
				return err
			}
			agg.deposit(1)
			_, err = h.accounts.Save(ctx, agg)
			return err
		}, eventstore.ErrStreamMismatch},
		{"update of another stream", func(ctx context.Context) error {
			_, err := h.accounts.Update(ctx, other, deposit(1))
			return err
		}, eventstore.ErrStreamMismatch},
		{"create of another stream", func(ctx context.Context) error {
			_, err := h.accounts.Create(ctx, newAccountID(), deposit(1))
			return err
		}, eventstore.ErrStreamMismatch},
		{"within another stream", func(ctx context.Context) error {
			return uow.Within(ctx, uuid.UUID(other), func(context.Context) error { return nil })
		}, eventstore.ErrStreamMismatch},
		{"same stream under another tenant", func(ctx context.Context) error {
			_, err := h.accounts.Update(ctxutil.WithTenantID(ctx, "t2"), id, deposit(1))
			return err
		}, eventstore.ErrStreamMismatch},
		{"nested execute for the same stream", func(ctx context.Context) error {
			return uow.Execute(ctx, uuid.UUID(id), func(context.Context) error { return nil })
		}, eventstore.ErrNestedUnitOfWork},
		{"nested execute for another stream", func(ctx context.Context) error {
			return uow.Execute(ctx, uuid.UUID(other), func(context.Context) error { return nil })
		}, eventstore.ErrNestedUnitOfWork},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var inner error
			err := uow.Execute(ctx, uuid.UUID(id), func(ctx context.Context) error {
				inner = tt.fn(ctx)
				return inner
			})
			wantErr(t, inner, tt.want)
			wantErr(t, err, tt.want)
		})
	}

	for _, stream := range []accountID{id, other} {
		got, err := h.accounts.Load(ctx, stream)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Version() != 1 {
			t.Fatalf("stream %v at version %d after failed commands, want 1", uuid.UUID(stream), got.Version())
		}
	}
}

func (s suite) withinJoins(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := newAccountID()
	uow := h.backend.UnitOfWork

	t.Run("commits all writes together", func(t *testing.T) {
		var last eventstore.CommandResult
		err := uow.Execute(ctx, uuid.UUID(id), func(ctx context.Context) error {
			if _, err := h.accounts.Create(ctx, id, func(_ context.Context, a *account) error {
				a.open("alice")
				return nil
			}); err != nil {
				return err
			}
			if _, err := h.accounts.Create(ctx, id, deposit(1)); !errors.Is(err, eventstore.ErrAlreadyExists) {
				t.Errorf("second Create in the transaction: %v, want ErrAlreadyExists", err)
			}
			if _, err := h.accounts.Update(ctx, id, deposit(2), eventstore.ExpectVersion(1)); err != nil {
				return err
			}
			agg, err := h.accounts.Load(ctx, id)
			if err != nil {
				return err
			}
			agg.deposit(3)
			last, err = h.accounts.Save(ctx, agg)
			return err
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		got, err := h.accounts.Load(ctx, id)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Version() != 3 || got.balance != 5 || last.Version != 3 {
			t.Fatalf("version %d balance %d result %+v, want 3, 5 and version 3", got.Version(), got.balance, last)
		}
	})

	t.Run("opens a transaction outside one", func(t *testing.T) {
		err := uow.Within(ctx, uuid.UUID(id), func(ctx context.Context) error {
			_, err := h.accounts.Update(ctx, id, deposit(1))
			return err
		})
		if err != nil {
			t.Fatalf("Within: %v", err)
		}
	})

	t.Run("rolls back all writes together", func(t *testing.T) {
		err := uow.Execute(ctx, uuid.UUID(id), func(ctx context.Context) error {
			if _, err := h.accounts.Update(ctx, id, deposit(100)); err != nil {
				return err
			}
			return errRejected
		})
		wantErr(t, err, errRejected)
		got, err := h.accounts.Load(ctx, id)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Version() != 4 || got.balance != 6 {
			t.Fatalf("version %d balance %d after rollback, want 4 and 6", got.Version(), got.balance)
		}
	})
}

func (s suite) failedCommandRollsBack(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	id := h.openAccount(t, ctx)
	key := guard.Key{Rule: "account.email", Value: "alice@example.com"}
	seq := numbering.Sequence{Context: "account", Name: "statement"}

	tests := []struct {
		name string
		fn   func(ctx context.Context, a *account) error
	}{
		{"error", func(context.Context, *account) error { return errRejected }},
		{"error after raising", func(_ context.Context, a *account) error {
			a.deposit(1)
			return errRejected
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.accounts.Update(ctx, id, func(ctx context.Context, a *account) error {
				if err := h.backend.Guard.Claim(ctx, key); err != nil {
					return err
				}
				if _, err := h.backend.Numbering.Next(ctx, seq); err != nil {
					return err
				}
				return tt.fn(ctx, a)
			})
			wantErr(t, err, errRejected)
		})
	}

	got, err := h.accounts.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version() != 1 {
		t.Fatalf("version %d after failed commands, want 1", got.Version())
	}
	if _, err := h.accounts.Update(ctx, id, func(ctx context.Context, _ *account) error {
		if err := h.backend.Guard.Claim(ctx, key); err != nil {
			return err
		}
		n, err := h.backend.Numbering.Next(ctx, seq)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("Next = %d after rolled-back draws, want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("Update after rollbacks: %v", err)
	}
}
