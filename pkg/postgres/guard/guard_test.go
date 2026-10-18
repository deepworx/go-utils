package guard_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/guard"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

type claimer interface {
	Claim(ctx context.Context, key guard.Key) error
}

type variant struct {
	name  string
	setup func(t *testing.T) (claimer, postgres.UnitOfWork)
}

func variants() []variant {
	return []variant{
		{name: "pg", setup: func(t *testing.T) (claimer, postgres.UnitOfWork) {
			pool := pgtest.New(t)
			return guard.NewPgStore(pool), postgres.NewUnitOfWork(pool)
		}},
		{name: "in-memory", setup: func(*testing.T) (claimer, postgres.UnitOfWork) {
			return guard.NewInMemoryStore(), postgres.NewInMemoryUnitOfWork()
		}},
	}
}

func tenant(id string) context.Context {
	return ctxutil.WithTenantID(context.Background(), id)
}

func TestClaim(t *testing.T) {
	t.Parallel()

	email := guard.Key{Rule: "user.email", Value: "a@example.com"}

	type claim struct {
		ctx     context.Context
		key     guard.Key
		wantErr error
	}
	tests := []struct {
		name   string
		claims []claim
	}{
		{
			name:   "free key succeeds",
			claims: []claim{{ctx: tenant("t1"), key: email}},
		},
		{
			name: "same key under two tenants succeeds",
			claims: []claim{
				{ctx: tenant("t1"), key: email},
				{ctx: tenant("t2"), key: email},
			},
		},
		{
			name: "same key under one tenant is taken",
			claims: []claim{
				{ctx: tenant("t1"), key: email},
				{ctx: tenant("t1"), key: email, wantErr: guard.ErrTaken},
			},
		},
		{
			name: "same value under another rule succeeds",
			claims: []claim{
				{ctx: tenant("t1"), key: email},
				{ctx: tenant("t1"), key: guard.Key{Rule: "user.login", Value: email.Value}},
			},
		},
		{
			name:   "missing tenant fails",
			claims: []claim{{ctx: context.Background(), key: email, wantErr: guard.ErrNoTenant}},
		},
		{
			name:   "empty rule fails",
			claims: []claim{{ctx: tenant("t1"), key: guard.Key{Value: "x"}, wantErr: guard.ErrEmptyKey}},
		},
		{
			name:   "empty value fails",
			claims: []claim{{ctx: tenant("t1"), key: guard.Key{Rule: "r"}, wantErr: guard.ErrEmptyKey}},
		},
	}

	for _, v := range variants() {
		for _, tt := range tests {
			t.Run(v.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				store, _ := v.setup(t)
				for i, c := range tt.claims {
					if err := store.Claim(c.ctx, c.key); !errors.Is(err, c.wantErr) {
						t.Fatalf("claim %d: got %v, want %v", i, err, c.wantErr)
					}
				}
			})
		}
	}
}

func TestPgStoreWithSchema(t *testing.T) {
	t.Parallel()

	pool := pgtest.New(t)
	if _, err := pool.Exec(context.Background(), `CREATE SCHEMA alt;
		CREATE TABLE alt.guards (LIKE public.guards INCLUDING ALL)`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	key := guard.Key{Rule: "user.email", Value: "a@example.com"}
	ctx := tenant("t1")

	alt := guard.NewPgStore(pool, guard.WithSchema("alt"))
	if err := alt.Claim(ctx, key); err != nil {
		t.Fatalf("alt claim: %v", err)
	}
	if err := alt.Claim(ctx, key); !errors.Is(err, guard.ErrTaken) {
		t.Fatalf("alt reclaim: got %v, want %v", err, guard.ErrTaken)
	}
	if err := guard.NewPgStore(pool).Claim(ctx, key); err != nil {
		t.Fatalf("public claim: %v", err)
	}
}

func TestClaimInUnitOfWork(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	key := guard.Key{Rule: "order.number", Value: "42"}

	tests := []struct {
		name        string
		body        func(ctx context.Context, store claimer) error
		wantErr     error
		wantReclaim error
	}{
		{
			name: "commit keeps the claim",
			body: func(ctx context.Context, store claimer) error {
				return store.Claim(ctx, key)
			},
			wantReclaim: guard.ErrTaken,
		},
		{
			name: "rollback releases the claim",
			body: func(ctx context.Context, store claimer) error {
				if err := store.Claim(ctx, key); err != nil {
					return err
				}
				return errBoom
			},
			wantErr: errBoom,
		},
		{
			name: "taken key inside the transaction leaves it usable",
			body: func(ctx context.Context, store claimer) error {
				if err := store.Claim(ctx, key); err != nil {
					return err
				}
				if err := store.Claim(ctx, key); !errors.Is(err, guard.ErrTaken) {
					return errors.Join(errors.New("second claim not taken"), err)
				}
				return store.Claim(ctx, guard.Key{Rule: key.Rule, Value: "43"})
			},
			wantReclaim: guard.ErrTaken,
		},
	}

	for _, v := range variants() {
		for _, tt := range tests {
			t.Run(v.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				store, uow := v.setup(t)
				ctx := tenant("t1")
				err := uow.Execute(ctx, func(ctx context.Context) error { return tt.body(ctx, store) })
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("execute: got %v, want %v", err, tt.wantErr)
				}
				if err := store.Claim(ctx, key); !errors.Is(err, tt.wantReclaim) {
					t.Fatalf("reclaim: got %v, want %v", err, tt.wantReclaim)
				}
			})
		}
	}
}
