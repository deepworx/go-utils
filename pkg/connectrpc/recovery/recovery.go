// Package recovery provides panic recovery for Connect RPC handlers.
package recovery

import (
	"context"
	"errors"
	"log/slog"
	"runtime"

	"connectrpc.com/connect"

	"github.com/deepworx/go-utils/pkg/ctxutil"
)

// NewInterceptor creates a Connect RPC interceptor that recovers from panics.
// It catches panics in handlers, logs them with stack traces, and returns
// a connect.CodeInternal error to the client.
func NewInterceptor() connect.Interceptor {
	return &interceptor{}
}

type interceptor struct{}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (resp connect.AnyResponse, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = recoverPanic(ctx, req.Spec().Procedure, r)
			}
		}()
		return next(ctx, req)
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = recoverPanic(ctx, conn.Spec().Procedure, r)
			}
		}()
		return next(ctx, conn)
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

	if reqID, ok := ctxutil.RequestID(ctx); ok {
		attrs = append(attrs, slog.String("request_id", reqID))
	}

	slog.ErrorContext(ctx, "panic recovered", attrs...)

	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
