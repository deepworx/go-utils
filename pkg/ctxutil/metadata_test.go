package ctxutil

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestTenantID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		setup  func() context.Context
		wantID string
		wantOK bool
	}{
		{
			name:  "empty context",
			setup: context.Background,
		},
		{
			name: "explicit only",
			setup: func() context.Context {
				return WithTenantID(context.Background(), "tenant-explicit")
			},
			wantID: "tenant-explicit",
			wantOK: true,
		},
		{
			name: "claims only",
			setup: func() context.Context {
				return WithClaims(context.Background(), Claims{TenantID: "tenant-claims"})
			},
			wantID: "tenant-claims",
			wantOK: true,
		},
		{
			name: "explicit wins over claims",
			setup: func() context.Context {
				ctx := WithClaims(context.Background(), Claims{TenantID: "tenant-claims"})
				return WithTenantID(ctx, "tenant-explicit")
			},
			wantID: "tenant-explicit",
			wantOK: true,
		},
		{
			name: "empty explicit falls back to claims",
			setup: func() context.Context {
				ctx := WithClaims(context.Background(), Claims{TenantID: "tenant-claims"})
				return WithTenantID(ctx, "")
			},
			wantID: "tenant-claims",
			wantOK: true,
		},
		{
			name: "empty explicit and empty claims tenant",
			setup: func() context.Context {
				ctx := WithClaims(context.Background(), Claims{UserID: "user-1"})
				return WithTenantID(ctx, "")
			},
		},
		{
			name: "claims without tenant",
			setup: func() context.Context {
				return WithClaims(context.Background(), Claims{UserID: "user-1"})
			},
		},
		{
			name: "overwrite explicit",
			setup: func() context.Context {
				ctx := WithTenantID(context.Background(), "tenant-a")
				return WithTenantID(ctx, "tenant-b")
			},
			wantID: "tenant-b",
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotID, gotOK := TenantID(tt.setup())
			if gotID != tt.wantID || gotOK != tt.wantOK {
				t.Errorf("TenantID() = %q, %v, want %q, %v", gotID, gotOK, tt.wantID, tt.wantOK)
			}
		})
	}
}

func TestCausationID(t *testing.T) {
	t.Parallel()

	eventID := uuid.MustParse("01920000-0000-7000-8000-000000000001")
	otherID := uuid.MustParse("01920000-0000-7000-8000-000000000002")

	tests := []struct {
		name   string
		setup  func() context.Context
		wantID uuid.UUID
		wantOK bool
	}{
		{
			name:  "empty context",
			setup: context.Background,
		},
		{
			name: "round-trips event ID",
			setup: func() context.Context {
				return WithCausationID(context.Background(), eventID)
			},
			wantID: eventID,
			wantOK: true,
		},
		{
			name: "nil UUID is absent",
			setup: func() context.Context {
				return WithCausationID(context.Background(), uuid.Nil)
			},
		},
		{
			name: "nil UUID hides previous value",
			setup: func() context.Context {
				return WithCausationID(WithCausationID(context.Background(), eventID), uuid.Nil)
			},
		},
		{
			name: "overwrite",
			setup: func() context.Context {
				return WithCausationID(WithCausationID(context.Background(), eventID), otherID)
			},
			wantID: otherID,
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotID, gotOK := CausationID(tt.setup())
			if gotID != tt.wantID || gotOK != tt.wantOK {
				t.Errorf("CausationID() = %v, %v, want %v, %v", gotID, gotOK, tt.wantID, tt.wantOK)
			}
		})
	}
}

type stringSetter func(context.Context, string) context.Context

func TestStringMetadata(t *testing.T) {
	t.Parallel()

	accessors := []struct {
		name string
		with stringSetter
		get  func(context.Context) (string, bool)
	}{
		{name: "Actor", with: WithActor, get: Actor},
		{name: "CorrelationID", with: WithCorrelationID, get: CorrelationID},
	}

	tests := []struct {
		name      string
		setup     func(with stringSetter) context.Context
		wantValue string
		wantOK    bool
	}{
		{
			name:  "empty context",
			setup: func(stringSetter) context.Context { return context.Background() },
		},
		{
			name: "set",
			setup: func(with stringSetter) context.Context {
				return with(context.Background(), "value-1")
			},
			wantValue: "value-1",
			wantOK:    true,
		},
		{
			name: "empty string is absent",
			setup: func(with stringSetter) context.Context {
				return with(context.Background(), "")
			},
		},
		{
			name: "empty string hides previous value",
			setup: func(with stringSetter) context.Context {
				return with(with(context.Background(), "value-1"), "")
			},
		},
		{
			name: "overwrite",
			setup: func(with stringSetter) context.Context {
				return with(with(context.Background(), "value-1"), "value-2")
			},
			wantValue: "value-2",
			wantOK:    true,
		},
		{
			name: "claims are not consulted",
			setup: func(stringSetter) context.Context {
				return WithClaims(context.Background(), Claims{UserID: "user-1", TenantID: "tenant-1"})
			},
		},
	}

	for _, acc := range accessors {
		for _, tt := range tests {
			t.Run(acc.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				got, gotOK := acc.get(tt.setup(acc.with))
				if got != tt.wantValue || gotOK != tt.wantOK {
					t.Errorf("%s() = %q, %v, want %q, %v", acc.name, got, gotOK, tt.wantValue, tt.wantOK)
				}
			})
		}
	}
}
