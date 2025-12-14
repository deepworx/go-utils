package ctxutil

import (
	"context"
	"testing"
)

func TestRequestID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func() context.Context
		wantID  string
		wantOK  bool
	}{
		{
			name:   "empty context",
			setup:  context.Background,
			wantID: "",
			wantOK: false,
		},
		{
			name: "with request ID",
			setup: func() context.Context {
				return WithRequestID(context.Background(), "req-123")
			},
			wantID: "req-123",
			wantOK: true,
		},
		{
			name: "overwrite request ID",
			setup: func() context.Context {
				ctx := WithRequestID(context.Background(), "req-123")
				return WithRequestID(ctx, "req-456")
			},
			wantID: "req-456",
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := tt.setup()
			gotID, gotOK := RequestID(ctx)
			if gotID != tt.wantID {
				t.Errorf("RequestID() id = %v, want %v", gotID, tt.wantID)
			}
			if gotOK != tt.wantOK {
				t.Errorf("RequestID() ok = %v, want %v", gotOK, tt.wantOK)
			}
		})
	}
}

func TestClaims(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func() context.Context
		wantClaims Claims
		wantOK     bool
	}{
		{
			name:       "empty context",
			setup:      context.Background,
			wantClaims: Claims{},
			wantOK:     false,
		},
		{
			name: "full claims",
			setup: func() context.Context {
				return WithClaims(context.Background(), Claims{
					UserID:      "user-123",
					TenantID:    "tenant-456",
					Roles:       []string{"admin", "user"},
					Permissions: []string{"read", "write"},
				})
			},
			wantClaims: Claims{
				UserID:      "user-123",
				TenantID:    "tenant-456",
				Roles:       []string{"admin", "user"},
				Permissions: []string{"read", "write"},
			},
			wantOK: true,
		},
		{
			name: "partial claims",
			setup: func() context.Context {
				return WithClaims(context.Background(), Claims{
					UserID: "user-123",
				})
			},
			wantClaims: Claims{
				UserID: "user-123",
			},
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := tt.setup()
			gotClaims, gotOK := GetClaims(ctx)
			if gotOK != tt.wantOK {
				t.Errorf("GetClaims() ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotOK && gotClaims.UserID != tt.wantClaims.UserID {
				t.Errorf("GetClaims() UserID = %v, want %v", gotClaims.UserID, tt.wantClaims.UserID)
			}
			if gotOK && gotClaims.TenantID != tt.wantClaims.TenantID {
				t.Errorf("GetClaims() TenantID = %v, want %v", gotClaims.TenantID, tt.wantClaims.TenantID)
			}
		})
	}
}

func TestIndividualClaimAccessors(t *testing.T) {
	t.Parallel()

	ctx := WithClaims(context.Background(), Claims{
		UserID:      "user-123",
		TenantID:    "tenant-456",
		Roles:       []string{"admin"},
		Permissions: []string{"read"},
	})

	t.Run("UserID", func(t *testing.T) {
		t.Parallel()
		id, ok := UserID(ctx)
		if !ok || id != "user-123" {
			t.Errorf("UserID() = %v, %v, want user-123, true", id, ok)
		}
	})

	t.Run("TenantID", func(t *testing.T) {
		t.Parallel()
		id, ok := TenantID(ctx)
		if !ok || id != "tenant-456" {
			t.Errorf("TenantID() = %v, %v, want tenant-456, true", id, ok)
		}
	})

	t.Run("Roles", func(t *testing.T) {
		t.Parallel()
		roles, ok := Roles(ctx)
		if !ok || len(roles) != 1 || roles[0] != "admin" {
			t.Errorf("Roles() = %v, %v, want [admin], true", roles, ok)
		}
	})

	t.Run("Permissions", func(t *testing.T) {
		t.Parallel()
		perms, ok := Permissions(ctx)
		if !ok || len(perms) != 1 || perms[0] != "read" {
			t.Errorf("Permissions() = %v, %v, want [read], true", perms, ok)
		}
	})
}

func TestIndividualClaimAccessorsEmptyContext(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	if id, ok := UserID(ctx); ok || id != "" {
		t.Errorf("UserID() on empty context = %v, %v, want '', false", id, ok)
	}
	if id, ok := TenantID(ctx); ok || id != "" {
		t.Errorf("TenantID() on empty context = %v, %v, want '', false", id, ok)
	}
	if roles, ok := Roles(ctx); ok || roles != nil {
		t.Errorf("Roles() on empty context = %v, %v, want nil, false", roles, ok)
	}
	if perms, ok := Permissions(ctx); ok || perms != nil {
		t.Errorf("Permissions() on empty context = %v, %v, want nil, false", perms, ok)
	}
}
