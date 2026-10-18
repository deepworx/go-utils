package deadline

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/deepworx/go-utils/internal/connecttest"
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

func TestInterceptor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		cfg              Config
		streamType       connect.StreamType
		existingDeadline time.Duration
		wantDeadline     bool
		wantMin          time.Duration
		wantMax          time.Duration
	}{
		{
			name:         "unary applies default",
			cfg:          Config{DefaultTimeout: 100 * time.Millisecond},
			wantDeadline: true,
			wantMin:      80 * time.Millisecond,
			wantMax:      110 * time.Millisecond,
		},
		{
			name:             "unary caps existing deadline",
			cfg:              Config{DefaultTimeout: 50 * time.Millisecond, MaxTimeout: 100 * time.Millisecond},
			existingDeadline: 500 * time.Millisecond,
			wantDeadline:     true,
			wantMin:          80 * time.Millisecond,
			wantMax:          110 * time.Millisecond,
		},
		{
			name:       "server streaming passes through",
			cfg:        Config{DefaultTimeout: 100 * time.Millisecond},
			streamType: connect.StreamTypeServer,
		},
		{
			name:       "bidi streaming passes through",
			cfg:        Config{DefaultTimeout: 100 * time.Millisecond},
			streamType: connect.StreamTypeBidi,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			if tt.existingDeadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.existingDeadline)
				t.Cleanup(cancel)
			}

			var deadline time.Time
			var hasDeadline bool
			err := connecttest.Serve(ctx, connecttest.Call{
				StreamType: tt.streamType,
				Handler: func(ctx context.Context, _ connect.Spec, _ connect.ServerStream) error {
					deadline, hasDeadline = ctx.Deadline()
					return nil
				},
			}, NewInterceptor(tt.cfg))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if hasDeadline != tt.wantDeadline {
				t.Fatalf("has deadline = %v, want %v", hasDeadline, tt.wantDeadline)
			}
			if !tt.wantDeadline {
				return
			}
			remaining := time.Until(deadline)
			if remaining < tt.wantMin || remaining > tt.wantMax {
				t.Errorf("remaining time %v not in range [%v, %v]", remaining, tt.wantMin, tt.wantMax)
			}
		})
	}
}
