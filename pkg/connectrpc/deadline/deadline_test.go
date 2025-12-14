package deadline

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
)

func TestNewInterceptor_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cfg         Config
		shouldPanic bool
		panicMsg    string
	}{
		{
			name:        "valid config with only default",
			cfg:         Config{DefaultTimeout: 10 * time.Second},
			shouldPanic: false,
		},
		{
			name:        "valid config with max equal to default",
			cfg:         Config{DefaultTimeout: 10 * time.Second, MaxTimeout: 10 * time.Second},
			shouldPanic: false,
		},
		{
			name:        "valid config with max greater than default",
			cfg:         Config{DefaultTimeout: 10 * time.Second, MaxTimeout: 30 * time.Second},
			shouldPanic: false,
		},
		{
			name:        "zero default timeout",
			cfg:         Config{DefaultTimeout: 0},
			shouldPanic: true,
			panicMsg:    "DefaultTimeout must be positive",
		},
		{
			name:        "negative default timeout",
			cfg:         Config{DefaultTimeout: -1 * time.Second},
			shouldPanic: true,
			panicMsg:    "DefaultTimeout must be positive",
		},
		{
			name:        "max timeout less than default",
			cfg:         Config{DefaultTimeout: 10 * time.Second, MaxTimeout: 5 * time.Second},
			shouldPanic: true,
			panicMsg:    "MaxTimeout must be >= DefaultTimeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.shouldPanic {
				defer func() {
					r := recover()
					if r == nil {
						t.Error("expected panic, got none")
						return
					}
					msg, ok := r.(string)
					if !ok {
						t.Errorf("expected string panic, got %T", r)
						return
					}
					if !strings.Contains(msg, tt.panicMsg) {
						t.Errorf("panic message %q does not contain %q", msg, tt.panicMsg)
					}
				}()
			}

			NewInterceptor(tt.cfg)
		})
	}
}

func TestApplyDeadline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		defaultTimeout   time.Duration
		maxTimeout       time.Duration
		existingDeadline time.Duration
		wantMin          time.Duration
		wantMax          time.Duration
	}{
		{
			name:             "no existing deadline applies default",
			defaultTimeout:   100 * time.Millisecond,
			maxTimeout:       0,
			existingDeadline: 0,
			wantMin:          80 * time.Millisecond,
			wantMax:          110 * time.Millisecond,
		},
		{
			name:             "existing deadline within max keeps original",
			defaultTimeout:   100 * time.Millisecond,
			maxTimeout:       200 * time.Millisecond,
			existingDeadline: 150 * time.Millisecond,
			wantMin:          130 * time.Millisecond,
			wantMax:          160 * time.Millisecond,
		},
		{
			name:             "existing deadline exceeds max gets capped",
			defaultTimeout:   100 * time.Millisecond,
			maxTimeout:       200 * time.Millisecond,
			existingDeadline: 500 * time.Millisecond,
			wantMin:          180 * time.Millisecond,
			wantMax:          210 * time.Millisecond,
		},
		{
			name:             "existing deadline with no max keeps original",
			defaultTimeout:   100 * time.Millisecond,
			maxTimeout:       0,
			existingDeadline: 500 * time.Millisecond,
			wantMin:          480 * time.Millisecond,
			wantMax:          510 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			i := &interceptor{
				defaultTimeout: tt.defaultTimeout,
				maxTimeout:     tt.maxTimeout,
			}

			ctx := context.Background()
			if tt.existingDeadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.existingDeadline)
				defer cancel()
			}

			resultCtx, cancel := i.applyDeadline(ctx)
			defer cancel()

			deadline, ok := resultCtx.Deadline()
			if !ok {
				t.Fatal("expected deadline in context")
			}

			remaining := time.Until(deadline)
			if remaining < tt.wantMin || remaining > tt.wantMax {
				t.Errorf("remaining time %v not in range [%v, %v]",
					remaining, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestInterceptor_WrapUnary_ServerSide(t *testing.T) {
	t.Parallel()

	interceptor := NewInterceptor(Config{
		DefaultTimeout: 100 * time.Millisecond,
	})

	var capturedDeadline time.Time
	var hadDeadline bool

	wrapped := interceptor.WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		capturedDeadline, hadDeadline = ctx.Deadline()
		return &mockResponse{}, nil
	})

	req := &mockRequest{procedure: "/test.Service/Method", isClient: false}
	_, err := wrapped(context.Background(), req)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !hadDeadline {
		t.Error("expected deadline in handler context")
	}

	remaining := time.Until(capturedDeadline)
	if remaining < 80*time.Millisecond || remaining > 110*time.Millisecond {
		t.Errorf("deadline remaining %v, expected ~100ms", remaining)
	}
}

func TestInterceptor_WrapUnary_ClientSide_Passthrough(t *testing.T) {
	t.Parallel()

	interceptor := NewInterceptor(Config{
		DefaultTimeout: 100 * time.Millisecond,
	})

	var hadDeadline bool

	wrapped := interceptor.WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		_, hadDeadline = ctx.Deadline()
		return &mockResponse{}, nil
	})

	req := &mockRequest{procedure: "/test.Service/Method", isClient: true}
	_, err := wrapped(context.Background(), req)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if hadDeadline {
		t.Error("client call should not have deadline applied")
	}
}

func TestInterceptor_WrapStreamingHandler_Passthrough(t *testing.T) {
	t.Parallel()

	interceptor := NewInterceptor(Config{
		DefaultTimeout: 100 * time.Millisecond,
	})

	called := false
	original := func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		called = true
		_, hasDeadline := ctx.Deadline()
		if hasDeadline {
			t.Error("streaming handler should not have deadline applied")
		}
		return nil
	}

	wrapped := interceptor.WrapStreamingHandler(original)
	_ = wrapped(context.Background(), &mockStreamingConn{})

	if !called {
		t.Error("expected handler to be called")
	}
}

func TestInterceptor_WrapStreamingClient_Passthrough(t *testing.T) {
	t.Parallel()

	interceptor := NewInterceptor(Config{
		DefaultTimeout: 100 * time.Millisecond,
	})

	called := false
	original := func(_ context.Context, _ connect.Spec) connect.StreamingClientConn {
		called = true
		return nil
	}

	wrapped := interceptor.WrapStreamingClient(original)
	wrapped(context.Background(), connect.Spec{})

	if !called {
		t.Error("expected original function to be called")
	}
}

func TestInterceptor_WrapUnary_MaxTimeoutCapsExisting(t *testing.T) {
	t.Parallel()

	interceptor := NewInterceptor(Config{
		DefaultTimeout: 50 * time.Millisecond,
		MaxTimeout:     100 * time.Millisecond,
	})

	var capturedDeadline time.Time
	var hadDeadline bool

	wrapped := interceptor.WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		capturedDeadline, hadDeadline = ctx.Deadline()
		return &mockResponse{}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := &mockRequest{procedure: "/test.Service/Method", isClient: false}
	_, err := wrapped(ctx, req)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !hadDeadline {
		t.Error("expected deadline in handler context")
	}

	remaining := time.Until(capturedDeadline)
	if remaining < 80*time.Millisecond || remaining > 110*time.Millisecond {
		t.Errorf("deadline remaining %v, expected ~100ms (capped from 500ms)", remaining)
	}
}

type mockRequest struct {
	connect.AnyRequest
	procedure string
	isClient  bool
}

func (r *mockRequest) Spec() connect.Spec {
	return connect.Spec{Procedure: r.procedure, IsClient: r.isClient}
}

type mockResponse struct {
	connect.AnyResponse
}

type mockStreamingConn struct {
	connect.StreamingHandlerConn
}
