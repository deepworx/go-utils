// Package errors provides error mapping for Connect RPC handlers.
package errors

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"
)

// ConnectCoder allows errors to specify their Connect RPC error code.
// Implement this interface on domain errors to map them to appropriate
// Connect codes while preserving the original error message.
type ConnectCoder interface {
	ConnectCode() connect.Code
}

// NewInterceptor creates a Connect RPC server interceptor that maps errors to
// appropriate Connect codes.
//
// Error mapping priority:
//  1. context.Canceled → CodeCanceled
//  2. context.DeadlineExceeded → CodeDeadlineExceeded
//  3. ConnectCoder interface → code from ConnectCode()
//  4. *connect.Error → preserved as-is
//  5. Any other error → CodeInternal with message "internal error"
//
// For mapped errors (1-3), the original message is sent to the client.
// For unmapped errors (5), the message is sanitized to hide internal details.
// The original error stays available to errors.Is and errors.As as the cause.
func NewInterceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			if err := next(ctx, spec, stream); err != nil {
				return mapError(err)
			}
			return nil
		}
	}
}

func mapError(err error) *connect.Error {
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err.Error()).WithCause(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err.Error()).WithCause(err)
	}

	var coder ConnectCoder
	if errors.As(err, &coder) {
		return connect.NewError(coder.ConnectCode(), err.Error()).WithCause(err)
	}

	if connectErr, ok := errors.AsType[*connect.Error](err); ok {
		return connectErr
	}

	return connect.NewError(connect.CodeInternal, "internal error").WithCause(err)
}
