// Package deadline provides deadline enforcement for Connect RPC handlers.
package deadline

import (
	"context"
	"time"

	"connectrpc.com/connect/v2"
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

// NewInterceptor creates a Connect RPC server interceptor that enforces
// deadlines on unary RPCs. It applies DefaultTimeout when no deadline exists on
// the incoming context, and caps existing deadlines to MaxTimeout if configured.
// Streaming RPCs are long-lived and pass through unchanged.
//
// Panics if:
//   - DefaultTimeout <= 0
//   - MaxTimeout > 0 && MaxTimeout < DefaultTimeout
func NewInterceptor(cfg Config) connect.ServerInterceptor {
	if cfg.DefaultTimeout <= 0 {
		panic("deadline: DefaultTimeout must be positive")
	}
	if cfg.MaxTimeout > 0 && cfg.MaxTimeout < cfg.DefaultTimeout {
		panic("deadline: MaxTimeout must be >= DefaultTimeout when set")
	}
	i := &interceptor{
		defaultTimeout: cfg.DefaultTimeout,
		maxTimeout:     cfg.MaxTimeout,
	}
	return i.wrap
}

type interceptor struct {
	defaultTimeout time.Duration
	maxTimeout     time.Duration
}

func (i *interceptor) wrap(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		if spec.StreamType != connect.StreamTypeUnary {
			return next(ctx, spec, stream)
		}

		ctx, cancel := i.applyDeadline(ctx)
		defer cancel()

		return next(ctx, spec, stream)
	}
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
