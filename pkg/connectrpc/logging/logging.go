// Package logging provides structured request/response logging for Connect RPC handlers.
package logging

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect/v2"
)

// NewInterceptor creates a Connect RPC server interceptor that logs requests
// and responses. Successful requests are logged at Info level, errors at Warn
// level. Request-scoped fields such as request_id and user_id are added by
// slogutil.ContextHandler, not by this interceptor.
func NewInterceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			err := next(ctx, spec, stream)
			logRequest(ctx, spec.Procedure, err)
			return err
		}
	}
}

func logRequest(ctx context.Context, procedure string, err error) {
	attrs := []any{
		slog.String("procedure", procedure),
		slog.String("status", getStatus(err)),
	}

	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
		slog.WarnContext(ctx, "rpc failed", attrs...)
		return
	}

	slog.InfoContext(ctx, "rpc completed", attrs...)
}

func getStatus(err error) string {
	if err == nil {
		return "ok"
	}
	if connectErr, ok := errors.AsType[*connect.Error](err); ok {
		return connectErr.Code().String()
	}
	return "unknown"
}
