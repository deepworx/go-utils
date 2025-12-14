// Package errors provides error mapping for Connect RPC handlers.
package errors

import (
	"context"
	"errors"

	"connectrpc.com/connect"
)

// ConnectCoder allows errors to specify their Connect RPC error code.
// Implement this interface on domain errors to map them to appropriate
// Connect codes while preserving the original error message.
type ConnectCoder interface {
	ConnectCode() connect.Code
}

// NewInterceptor creates a Connect RPC interceptor that maps errors to
// appropriate Connect codes.
//
// Error mapping priority:
//  1. context.Canceled → CodeCanceled
//  2. context.DeadlineExceeded → CodeDeadlineExceeded
//  3. ConnectCoder interface → code from ConnectCode()
//  4. *connect.Error → preserved as-is
//  5. Any other error → CodeInternal with message "internal error"
//
// For mapped errors (1-4), the original message is preserved.
// For unmapped errors (5), the message is sanitized to hide internal details.
func NewInterceptor() connect.Interceptor {
	return &interceptor{}
}

type interceptor struct{}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		resp, err := next(ctx, req)
		if err != nil {
			return resp, mapError(err)
		}
		return resp, nil
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		err := next(ctx, conn)
		if err != nil {
			return mapError(err)
		}
		return nil
	}
}

func mapError(err error) *connect.Error {
	// Check context errors first
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}

	// Check if error implements ConnectCoder
	var coder ConnectCoder
	if errors.As(err, &coder) {
		return connect.NewError(coder.ConnectCode(), err)
	}

	// Check if already a connect.Error
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return connectErr
	}

	// Unmapped error: return CodeInternal with sanitized message
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
