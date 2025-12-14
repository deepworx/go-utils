// Package logging provides structured request/response logging for Connect RPC handlers.
package logging

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/deepworx/go-utils/pkg/ctxutil"
)

// NewInterceptor creates a Connect RPC interceptor that logs requests and responses.
// Successful requests are logged at Info level, errors at Warn level.
func NewInterceptor() connect.Interceptor {
	return &interceptor{}
}

type interceptor struct{}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		resp, err := next(ctx, req)
		logRequest(ctx, req.Spec().Procedure, err)
		return resp, err
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		err := next(ctx, conn)
		logRequest(ctx, conn.Spec().Procedure, err)
		return err
	}
}

func logRequest(ctx context.Context, procedure string, err error) {
	attrs := []any{
		slog.String("procedure", procedure),
		slog.String("status", getStatus(err)),
	}

	if reqID, ok := ctxutil.RequestID(ctx); ok {
		attrs = append(attrs, slog.String("request_id", reqID))
	}
	if userID, ok := ctxutil.UserID(ctx); ok {
		attrs = append(attrs, slog.String("user_id", userID))
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
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return connectErr.Code().String()
	}
	return "unknown"
}
