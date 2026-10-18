package interceptor

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/deepworx/go-utils/internal/connecttest"
	"github.com/deepworx/go-utils/pkg/connectrpc/deadline"
	"github.com/deepworx/go-utils/pkg/connectrpc/jwtauth"
	"github.com/deepworx/go-utils/pkg/connectrpc/requestid"
	"github.com/deepworx/go-utils/pkg/ctxutil"
)

func TestBuildDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		opts      []Option
		wantCount int
	}{
		{
			name:      "default config",
			opts:      nil,
			wantCount: 7,
		},
		{
			name: "with custom deadline",
			opts: []Option{
				WithDeadline(deadline.Config{
					DefaultTimeout: 60_000_000_000,
					MaxTimeout:     300_000_000_000,
				}),
			},
			wantCount: 7,
		},
		{
			name: "with custom requestID",
			opts: []Option{
				WithRequestID(requestid.Config{
					HeaderName: "X-Custom-Request-ID",
				}),
			},
			wantCount: 7,
		},
		{
			name: "with all options",
			opts: []Option{
				WithDeadline(deadline.Config{
					DefaultTimeout: 60_000_000_000,
					MaxTimeout:     300_000_000_000,
				}),
				WithRequestID(requestid.Config{
					HeaderName: "X-Custom-Request-ID",
				}),
			},
			wantCount: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			interceptors, err := BuildDefault(tt.opts...)
			if err != nil {
				t.Fatalf("BuildDefault() error = %v", err)
			}
			if len(interceptors) != tt.wantCount {
				t.Errorf("BuildDefault() returned %d interceptors, want %d", len(interceptors), tt.wantCount)
			}
		})
	}
}

func TestBuildDefaultWithAuth_NilAuth(t *testing.T) {
	t.Parallel()

	_, err := BuildDefaultWithAuth(nil)
	if err == nil {
		t.Fatal("BuildDefaultWithAuth(nil) should return error")
	}
}

func TestBuildDefaultWithAuth_ValidAuth(t *testing.T) {
	t.Parallel()

	// Create a mock authenticator for testing
	// Since we can't easily create a real Authenticator without JWKS,
	// we test that the function signature works with a non-nil pointer.
	// In integration tests, a real authenticator would be used.
	auth := &jwtauth.Authenticator{}

	interceptors, err := BuildDefaultWithAuth(auth)
	if err != nil {
		t.Fatalf("BuildDefaultWithAuth() error = %v", err)
	}
	if len(interceptors) != 8 {
		t.Errorf("BuildDefaultWithAuth() returned %d interceptors, want 8", len(interceptors))
	}
}

func TestBuildDefaultWithAuth_WithOptions(t *testing.T) {
	t.Parallel()

	auth := &jwtauth.Authenticator{}

	interceptors, err := BuildDefaultWithAuth(auth,
		WithDeadline(deadline.Config{
			DefaultTimeout: 60_000_000_000,
			MaxTimeout:     300_000_000_000,
		}),
		WithRequestID(requestid.Config{
			HeaderName: "X-Custom-Request-ID",
		}),
	)
	if err != nil {
		t.Fatalf("BuildDefaultWithAuth() error = %v", err)
	}
	if len(interceptors) != 8 {
		t.Errorf("BuildDefaultWithAuth() returned %d interceptors, want 8", len(interceptors))
	}
}

func TestBuildDefault_Serve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		handler     connect.ServerFunc
		wantCode    connect.Code
		wantMessage string
	}{
		{
			name: "success with request ID and deadline",
			handler: func(ctx context.Context, _ connect.Spec, _ connect.ServerStream) error {
				if id, _ := ctxutil.RequestID(ctx); id != "req-1" {
					return connect.Errorf(connect.CodeFailedPrecondition, "request ID = %q", id)
				}
				if _, ok := ctx.Deadline(); !ok {
					return connect.NewError(connect.CodeFailedPrecondition, "no deadline")
				}
				return nil
			},
		},
		{
			name: "unmapped error is sanitized",
			handler: func(context.Context, connect.Spec, connect.ServerStream) error {
				return errors.New("db password leaked")
			},
			wantCode:    connect.CodeInternal,
			wantMessage: "internal error",
		},
		{
			name: "panic is recovered",
			handler: func(context.Context, connect.Spec, connect.ServerStream) error {
				panic("boom")
			},
			wantCode:    connect.CodeInternal,
			wantMessage: "internal error",
		},
	}

	interceptors, err := BuildDefault()
	if err != nil {
		t.Fatalf("BuildDefault() error = %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := connecttest.Serve(context.Background(), connecttest.Call{
				Header:  map[string]string{"X-Request-ID": "req-1"},
				Handler: tt.handler,
			}, interceptors...)

			if tt.wantCode == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			connectErr, ok := errors.AsType[*connect.Error](err)
			if !ok {
				t.Fatalf("expected connect.Error, got %T", err)
			}
			if connectErr.Code() != tt.wantCode {
				t.Errorf("code = %v, want %v", connectErr.Code(), tt.wantCode)
			}
			if connectErr.Message() != tt.wantMessage {
				t.Errorf("message = %q, want %q", connectErr.Message(), tt.wantMessage)
			}
		})
	}
}
