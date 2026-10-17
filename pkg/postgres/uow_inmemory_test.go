package postgres

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestInMemoryUnitOfWorkStage(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")

	tests := []struct {
		name    string
		run     func(ctx context.Context, uow *InMemoryUnitOfWork, record func(string)) error
		wantErr error
		want    []string
	}{
		{
			name: "staged writes apply in order on commit",
			run: func(ctx context.Context, uow *InMemoryUnitOfWork, record func(string)) error {
				return uow.Execute(ctx, func(ctx context.Context) error {
					Stage(ctx, func() { record("first") })
					Stage(ctx, func() { record("second") })
					record("body done")
					return nil
				})
			},
			want: []string{"body done", "first", "second"},
		},
		{
			name: "staged writes are dropped on error",
			run: func(ctx context.Context, uow *InMemoryUnitOfWork, record func(string)) error {
				return uow.Execute(ctx, func(ctx context.Context) error {
					Stage(ctx, func() { record("first") })
					return errBoom
				})
			},
			wantErr: errBoom,
		},
		{
			name: "outside a transaction stage applies immediately",
			run: func(ctx context.Context, _ *InMemoryUnitOfWork, record func(string)) error {
				Stage(ctx, func() { record("first") })
				record("after stage")
				return nil
			},
			want: []string{"first", "after stage"},
		},
		{
			name: "nested execute joins the outer transaction",
			run: func(ctx context.Context, uow *InMemoryUnitOfWork, record func(string)) error {
				return uow.Execute(ctx, func(ctx context.Context) error {
					if err := uow.Execute(ctx, func(ctx context.Context) error {
						Stage(ctx, func() { record("inner") })
						return nil
					}); err != nil {
						return err
					}
					record("inner returned")
					return errBoom
				})
			},
			wantErr: errBoom,
			want:    []string{"inner returned"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got []string
			record := func(s string) { got = append(got, s) }

			err := tt.run(context.Background(), NewInMemoryUnitOfWork(), record)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("applied = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInMemoryUnitOfWorkDropsStagedWritesOnPanic(t *testing.T) {
	t.Parallel()

	applied := false
	defer func() {
		if recover() == nil {
			t.Fatal("panic was swallowed")
		}
		if applied {
			t.Fatal("staged write applied after panic")
		}
	}()

	_ = NewInMemoryUnitOfWork().Execute(context.Background(), func(ctx context.Context) error {
		Stage(ctx, func() { applied = true })
		panic("boom")
	})
}

func TestInMemoryUnitOfWorkOnRollback(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")

	tests := []struct {
		name    string
		run     func(ctx context.Context, uow *InMemoryUnitOfWork, record func(string)) error
		wantErr error
		want    []string
	}{
		{
			name: "undo runs in reverse order on error",
			run: func(ctx context.Context, uow *InMemoryUnitOfWork, record func(string)) error {
				return uow.Execute(ctx, func(ctx context.Context) error {
					OnRollback(ctx, func() { record("first") })
					OnRollback(ctx, func() { record("second") })
					return errBoom
				})
			},
			wantErr: errBoom,
			want:    []string{"second", "first"},
		},
		{
			name: "undo is dropped on commit",
			run: func(ctx context.Context, uow *InMemoryUnitOfWork, record func(string)) error {
				return uow.Execute(ctx, func(ctx context.Context) error {
					OnRollback(ctx, func() { record("undo") })
					return nil
				})
			},
		},
		{
			name: "outside a transaction undo never runs",
			run: func(ctx context.Context, _ *InMemoryUnitOfWork, record func(string)) error {
				OnRollback(ctx, func() { record("undo") })
				return nil
			},
		},
		{
			name: "nested execute undoes with the outer transaction",
			run: func(ctx context.Context, uow *InMemoryUnitOfWork, record func(string)) error {
				return uow.Execute(ctx, func(ctx context.Context) error {
					if err := uow.Execute(ctx, func(ctx context.Context) error {
						OnRollback(ctx, func() { record("inner") })
						return nil
					}); err != nil {
						return err
					}
					return errBoom
				})
			},
			wantErr: errBoom,
			want:    []string{"inner"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got []string
			record := func(s string) { got = append(got, s) }

			err := tt.run(context.Background(), NewInMemoryUnitOfWork(), record)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("undone = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInMemoryUnitOfWorkUndoesOnPanic(t *testing.T) {
	t.Parallel()

	undone := false
	defer func() {
		if recover() == nil {
			t.Fatal("panic was swallowed")
		}
		if !undone {
			t.Fatal("undo did not run after panic")
		}
	}()

	_ = NewInMemoryUnitOfWork().Execute(context.Background(), func(ctx context.Context) error {
		OnRollback(ctx, func() { undone = true })
		panic("boom")
	})
}
