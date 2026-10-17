package postgres

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m))
}

func newCounterPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.New(t)
	if _, err := pool.Exec(context.Background(), `CREATE TABLE uow_counter (n int NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return pool
}

func countRows(t *testing.T, q Querier) int {
	t.Helper()
	var n int
	if err := q.QueryRow(context.Background(), `SELECT count(*) FROM uow_counter`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func insertRow(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := Conn(ctx, pool).Exec(ctx, `INSERT INTO uow_counter (n) VALUES (1)`)
	return err
}

func TestPgUnitOfWorkExecute(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")

	tests := []struct {
		name     string
		fn       func(ctx context.Context, uow *PgUnitOfWork, pool *pgxpool.Pool) error
		wantErr  error
		wantRows int
	}{
		{
			name: "commits on success",
			fn: func(ctx context.Context, _ *PgUnitOfWork, pool *pgxpool.Pool) error {
				return insertRow(ctx, pool)
			},
			wantRows: 1,
		},
		{
			name: "rolls back on error",
			fn: func(ctx context.Context, _ *PgUnitOfWork, pool *pgxpool.Pool) error {
				if err := insertRow(ctx, pool); err != nil {
					return err
				}
				return errBoom
			},
			wantErr: errBoom,
		},
		{
			name: "nested execute joins the outer transaction",
			fn: func(ctx context.Context, uow *PgUnitOfWork, pool *pgxpool.Pool) error {
				outer := TxFromContext(ctx)
				if err := uow.Execute(ctx, func(ctx context.Context) error {
					if TxFromContext(ctx) != outer {
						return errors.New("inner execute opened a new transaction")
					}
					return insertRow(ctx, pool)
				}); err != nil {
					return err
				}
				return errBoom
			},
			wantErr: errBoom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			pool := newCounterPool(t)
			uow := NewUnitOfWork(pool)

			err := uow.Execute(ctx, func(ctx context.Context) error {
				return tt.fn(ctx, uow, pool)
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got := countRows(t, pool); got != tt.wantRows {
				t.Fatalf("rows = %d, want %d", got, tt.wantRows)
			}
		})
	}
}

func TestPgUnitOfWorkRollsBackOnPanic(t *testing.T) {
	t.Parallel()
	pool := newCounterPool(t)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic was swallowed")
			}
		}()
		_ = NewUnitOfWork(pool).Execute(context.Background(), func(ctx context.Context) error {
			if err := insertRow(ctx, pool); err != nil {
				return err
			}
			panic("boom")
		})
	}()

	if got := countRows(t, pool); got != 0 {
		t.Fatalf("rows = %d, want 0", got)
	}
}

func TestConn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newCounterPool(t)

	if got := Conn(ctx, pool); got != Querier(pool) {
		t.Fatalf("Conn outside a transaction = %T, want the pool", got)
	}
	if TxFromContext(ctx) != nil {
		t.Fatal("TxFromContext outside a transaction is not nil")
	}

	err := NewUnitOfWork(pool).Execute(ctx, func(ctx context.Context) error {
		tx := TxFromContext(ctx)
		if tx == nil {
			return errors.New("TxFromContext inside a transaction is nil")
		}
		if got := Conn(ctx, pool); got != Querier(tx) {
			return errors.New("Conn inside a transaction is not the transaction")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPgUnitOfWorkTxOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := pgtest.New(t)
	uow := NewUnitOfWork(pool, TxOptions(pgx.TxOptions{
		IsoLevel:   pgx.Serializable,
		AccessMode: pgx.ReadOnly,
	}))

	err := uow.Execute(ctx, func(ctx context.Context) error {
		var iso, readOnly string
		if err := Conn(ctx, pool).QueryRow(ctx,
			`SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only')`,
		).Scan(&iso, &readOnly); err != nil {
			return err
		}
		if iso != "serializable" || readOnly != "on" {
			t.Errorf("isolation = %q, read only = %q, want serializable and on", iso, readOnly)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPgUnitOfWorkTenantSetting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []UnitOfWorkOption
		want string
	}{
		{name: "set with SetLocalTenant", opts: []UnitOfWorkOption{SetLocalTenant()}, want: "tenant-a"},
		{name: "unset by default", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pool := pgtest.New(t)
			ctx := ctxutil.WithTenantID(context.Background(), "tenant-a")

			var got string
			err := NewUnitOfWork(pool, tt.opts...).Execute(ctx, func(ctx context.Context) error {
				return Conn(ctx, pool).QueryRow(ctx,
					`SELECT coalesce(current_setting('app.tenant_id', true), '')`,
				).Scan(&got)
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("app.tenant_id = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPgUnitOfWorkTenantSettingIsTransactionLocal(t *testing.T) {
	t.Parallel()
	ctx := ctxutil.WithTenantID(context.Background(), "tenant-a")

	cfg := pgtest.New(t).Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	if err := NewUnitOfWork(pool, SetLocalTenant()).Execute(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}

	var got string
	if err := pool.QueryRow(ctx, `SELECT coalesce(current_setting('app.tenant_id', true), '')`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("app.tenant_id after commit = %q, want unset", got)
	}
}
