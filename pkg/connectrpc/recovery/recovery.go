// Package recovery provides panic recovery for Connect RPC handlers.
package recovery

import (
	"context"
	"log/slog"
	"runtime"

	"connectrpc.com/connect/v2"
)

// NewInterceptor creates a Connect RPC server interceptor that recovers from
// panics. It catches panics in handlers, logs them with stack traces, and
// returns a connect.CodeInternal error to the client. Request-scoped fields
// such as request_id are added by slogutil.ContextHandler, not by this
// interceptor.
func NewInterceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = recoverPanic(ctx, spec.Procedure, r)
				}
			}()
			return next(ctx, spec, stream)
		}
	}
}

func recoverPanic(ctx context.Context, procedure string, r any) *connect.Error {
	const stackSize = 4096
	stack := make([]byte, stackSize)
	n := runtime.Stack(stack, false)
	stackStr := string(stack[:n])

	attrs := []any{
		slog.String("procedure", procedure),
		slog.Any("panic", r),
		slog.String("stack", stackStr),
	}
	slog.ErrorContext(ctx, "panic recovered", attrs...)

	return connect.NewError(connect.CodeInternal, "internal error")
}
