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
