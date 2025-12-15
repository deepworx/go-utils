package interceptor

import (
	"testing"

	"github.com/deepworx/go-utils/pkg/connectrpc/deadline"
	"github.com/deepworx/go-utils/pkg/connectrpc/jwtauth"
	"github.com/deepworx/go-utils/pkg/connectrpc/requestid"
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
