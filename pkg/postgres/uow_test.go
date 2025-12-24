package postgres

import (
	"context"
	"errors"
	"testing"
)

func TestInMemoryUnitOfWork_Execute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fn      func(ctx context.Context, tx Transaction) error
		wantErr error
	}{
		{
			name:    "success",
			fn:      func(ctx context.Context, tx Transaction) error { return nil },
			wantErr: nil,
		},
		{
			name:    "error propagates",
			fn:      func(ctx context.Context, tx Transaction) error { return errors.New("test error") },
			wantErr: errors.New("test error"),
		},
		{
			name: "tx returns nil",
			fn: func(ctx context.Context, tx Transaction) error {
				if tx.Tx() != nil {
					return errors.New("expected nil from tx.Tx()")
				}
				return nil
			},
			wantErr: nil,
		},
		{
			name: "context is passed through",
			fn: func(ctx context.Context, tx Transaction) error {
				if ctx.Value(testContextKey("test")) != "value" {
					return errors.New("context not passed")
				}
				return nil
			},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			uow := NewInMemoryUnitOfWork()
			ctx := context.WithValue(context.Background(), testContextKey("test"), "value")
			err := uow.Execute(ctx, tt.fn)

			if tt.wantErr == nil && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.wantErr != nil && (err == nil || err.Error() != tt.wantErr.Error()) {
				t.Errorf("got error %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// testContextKey avoids staticcheck warning about string context keys.
type testContextKey string

func TestUnitOfWork_InterfaceSatisfaction(t *testing.T) {
	t.Parallel()
	var _ UnitOfWork = (*PgUnitOfWork)(nil)
	var _ UnitOfWork = (*InMemoryUnitOfWork)(nil)
}

func TestTransaction_InterfaceSatisfaction(t *testing.T) {
	t.Parallel()
	var _ Transaction = (*pgTransaction)(nil)
	var _ Transaction = nilTransaction{}
}
