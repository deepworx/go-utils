package numbering_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/numbering"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

type sequencer interface {
	Next(ctx context.Context, seq numbering.Sequence) (int64, error)
}

type variant struct {
	name  string
	setup func(t *testing.T) (sequencer, postgres.UnitOfWork)
}

func variants() []variant {
	return []variant{
		{name: "pg", setup: func(t *testing.T) (sequencer, postgres.UnitOfWork) {
			pool := pgtest.New(t)
			return numbering.NewPgStore(pool), postgres.NewUnitOfWork(pool)
		}},
		{name: "in-memory", setup: func(*testing.T) (sequencer, postgres.UnitOfWork) {
			return numbering.NewInMemoryStore(), postgres.NewInMemoryUnitOfWork()
		}},
	}
}

func tenant(id string) context.Context {
	return ctxutil.WithTenantID(context.Background(), id)
}

func TestNext(t *testing.T) {
	t.Parallel()

	invoices := numbering.Sequence{Context: "billing", Name: "invoice"}

	type call struct {
		ctx     context.Context
		seq     numbering.Sequence
		want    int64
		wantErr error
	}
	tests := []struct {
		name  string
		calls []call
	}{
		{
			name: "numbers start at one and increase by one",
			calls: []call{
				{ctx: tenant("t1"), seq: invoices, want: 1},
				{ctx: tenant("t1"), seq: invoices, want: 2},
				{ctx: tenant("t1"), seq: invoices, want: 3},
			},
		},
		{
			name: "each tenant has its own range",
			calls: []call{
				{ctx: tenant("t1"), seq: invoices, want: 1},
				{ctx: tenant("t2"), seq: invoices, want: 1},
				{ctx: tenant("t1"), seq: invoices, want: 2},
			},
		},
		{
			name: "each context and name has its own range",
			calls: []call{
				{ctx: tenant("t1"), seq: invoices, want: 1},
				{ctx: tenant("t1"), seq: numbering.Sequence{Context: "billing", Name: "credit-note"}, want: 1},
				{ctx: tenant("t1"), seq: numbering.Sequence{Context: "sales", Name: "invoice"}, want: 1},
			},
		},
		{
			name:  "missing tenant fails",
			calls: []call{{ctx: context.Background(), seq: invoices, wantErr: numbering.ErrNoTenant}},
		},
		{
			name:  "empty context fails",
			calls: []call{{ctx: tenant("t1"), seq: numbering.Sequence{Name: "invoice"}, wantErr: numbering.ErrEmptySequence}},
		},
		{
			name:  "empty name fails",
			calls: []call{{ctx: tenant("t1"), seq: numbering.Sequence{Context: "billing"}, wantErr: numbering.ErrEmptySequence}},
		},
	}

	for _, v := range variants() {
		for _, tt := range tests {
			t.Run(v.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				store, _ := v.setup(t)
				for i, c := range tt.calls {
					got, err := store.Next(c.ctx, c.seq)
					if !errors.Is(err, c.wantErr) {
						t.Fatalf("call %d: err = %v, want %v", i, err, c.wantErr)
					}
					if got != c.want {
						t.Fatalf("call %d: got %d, want %d", i, got, c.want)
					}
				}
			})
		}
	}
}

func TestNextInUnitOfWork(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	seq := numbering.Sequence{Context: "billing", Name: "invoice"}

	tests := []struct {
		name     string
		body     func(ctx context.Context, store sequencer) error
		wantErr  error
		wantNext int64
	}{
		{
			name: "commit keeps the numbers",
			body: func(ctx context.Context, store sequencer) error {
				return nextN(ctx, store, seq, 2)
			},
			wantNext: 3,
		},
		{
			name: "rollback leaves no gap",
			body: func(ctx context.Context, store sequencer) error {
				if err := nextN(ctx, store, seq, 2); err != nil {
					return err
				}
				return errBoom
			},
			wantErr:  errBoom,
			wantNext: 1,
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
				got, err := store.Next(ctx, seq)
				if err != nil {
					t.Fatalf("next: %v", err)
				}
				if got != tt.wantNext {
					t.Fatalf("next = %d, want %d", got, tt.wantNext)
				}
			})
		}
	}
}

func TestNextConcurrentTransactionsAreGaplessAndUnique(t *testing.T) {
	t.Parallel()

	const workers = 20
	seq := numbering.Sequence{Context: "billing", Name: "invoice"}

	for _, v := range variants() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()

			store, uow := v.setup(t)
			ctx := tenant("t1")

			var (
				mu   sync.Mutex
				got  []int64
				wg   sync.WaitGroup
				errs = make(chan error, workers)
			)
			for range workers {
				wg.Go(func() {
					errs <- uow.Execute(ctx, func(ctx context.Context) error {
						n, err := store.Next(ctx, seq)
						if err != nil {
							return err
						}
						mu.Lock()
						got = append(got, n)
						mu.Unlock()
						return nil
					})
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("execute: %v", err)
				}
			}

			slices.Sort(got)
			want := make([]int64, workers)
			for i := range want {
				want[i] = int64(i + 1)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("numbers = %v, want %v", got, want)
			}
		})
	}
}

func TestPgNextConcurrentRollbacksLeaveNoGap(t *testing.T) {
	t.Parallel()

	const workers = 20
	seq := numbering.Sequence{Context: "billing", Name: "invoice"}
	errRollback := errors.New("rollback")

	pool := pgtest.New(t)
	store, uow := numbering.NewPgStore(pool), postgres.NewUnitOfWork(pool)
	ctx := tenant("t1")

	var (
		mu  sync.Mutex
		got []int64
		wg  sync.WaitGroup
	)
	errs := make(chan error, workers)
	for i := range workers {
		wg.Go(func() {
			errs <- uow.Execute(ctx, func(ctx context.Context) error {
				n, err := store.Next(ctx, seq)
				if err != nil {
					return err
				}
				if i%2 == 1 {
					return errRollback
				}
				mu.Lock()
				got = append(got, n)
				mu.Unlock()
				return nil
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, errRollback) {
			t.Fatalf("execute: %v", err)
		}
	}

	slices.Sort(got)
	want := make([]int64, workers/2)
	for i := range want {
		want[i] = int64(i + 1)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("committed numbers = %v, want %v", got, want)
	}
}

func TestPgStoreWithSchema(t *testing.T) {
	t.Parallel()

	pool := pgtest.New(t)
	if _, err := pool.Exec(context.Background(), `CREATE SCHEMA alt;
		CREATE TABLE alt.identifier_sequences (LIKE public.identifier_sequences INCLUDING ALL)`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	seq := numbering.Sequence{Context: "billing", Name: "invoice"}
	ctx := tenant("t1")

	alt := numbering.NewPgStore(pool, numbering.WithSchema("alt"))
	for _, want := range []int64{1, 2} {
		if got, err := alt.Next(ctx, seq); err != nil || got != want {
			t.Fatalf("alt next = %d, %v; want %d", got, err, want)
		}
	}
	if got, err := numbering.NewPgStore(pool).Next(ctx, seq); err != nil || got != 1 {
		t.Fatalf("public next = %d, %v; want 1", got, err)
	}
}

func nextN(ctx context.Context, store sequencer, seq numbering.Sequence, n int) error {
	for range n {
		if _, err := store.Next(ctx, seq); err != nil {
			return err
		}
	}
	return nil
}
