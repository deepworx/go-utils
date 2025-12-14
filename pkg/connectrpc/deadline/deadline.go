// Package deadline provides deadline enforcement for Connect RPC handlers.
package deadline

import (
	"context"
	"time"

	"connectrpc.com/connect"
)

// Config holds configuration for the deadline interceptor.
type Config struct {
	// DefaultTimeout is applied when the incoming context has no deadline.
	// Must be positive (> 0).
	DefaultTimeout time.Duration `koanf:"default_timeout"`

	// MaxTimeout caps existing deadlines to min(existingDeadline, MaxTimeout).
	// Zero means no cap is applied (only DefaultTimeout is used).
	// If positive, must be >= DefaultTimeout.
	MaxTimeout time.Duration `koanf:"max_timeout"`
}

// DefaultConfig returns a Config with sensible default values.
func DefaultConfig() Config {
	return Config{
		DefaultTimeout: 30 * time.Second,
		MaxTimeout:     300 * time.Second,
	}
}

// NewInterceptor creates a Connect RPC interceptor that enforces deadlines.
// It applies DefaultTimeout when no deadline exists on the incoming context,
// and caps existing deadlines to MaxTimeout if configured.
//
// Panics if:
//   - DefaultTimeout <= 0
//   - MaxTimeout > 0 && MaxTimeout < DefaultTimeout
func NewInterceptor(cfg Config) connect.Interceptor {
	if cfg.DefaultTimeout <= 0 {
		panic("deadline: DefaultTimeout must be positive")
	}
	if cfg.MaxTimeout > 0 && cfg.MaxTimeout < cfg.DefaultTimeout {
		panic("deadline: MaxTimeout must be >= DefaultTimeout when set")
	}
	return &interceptor{
		defaultTimeout: cfg.DefaultTimeout,
		maxTimeout:     cfg.MaxTimeout,
	}
}

type interceptor struct {
	defaultTimeout time.Duration
	maxTimeout     time.Duration
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}

		ctx, cancel := i.applyDeadline(ctx)
		defer cancel()

		return next(ctx, req)
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// applyDeadline returns a context with an appropriate deadline and a cancel function.
func (i *interceptor) applyDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, hasDeadline := ctx.Deadline()

	if !hasDeadline {
		return context.WithTimeout(ctx, i.defaultTimeout)
	}

	if i.maxTimeout == 0 {
		return ctx, func() {}
	}

	maxDeadline := time.Now().Add(i.maxTimeout)
	if deadline.After(maxDeadline) {
		return context.WithDeadline(ctx, maxDeadline)
	}

	return ctx, func() {}
}
