package errors

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/deepworx/go-utils/internal/connecttest"
)

// codedError implements ConnectCoder for testing.
type codedError struct {
	msg  string
	code connect.Code
}

func (e *codedError) Error() string {
	return e.msg
}

func (e *codedError) ConnectCode() connect.Code {
	return e.code
}

func TestMapError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		err         error
		wantCode    connect.Code
		wantMessage string
	}{
		{
			name:        "context.Canceled",
			err:         context.Canceled,
			wantCode:    connect.CodeCanceled,
			wantMessage: "context canceled",
		},
		{
			name:        "wrapped context.Canceled",
			err:         fmt.Errorf("operation failed: %w", context.Canceled),
			wantCode:    connect.CodeCanceled,
			wantMessage: "operation failed: context canceled",
		},
		{
			name:        "context.DeadlineExceeded",
			err:         context.DeadlineExceeded,
			wantCode:    connect.CodeDeadlineExceeded,
			wantMessage: "context deadline exceeded",
		},
		{
			name:        "wrapped context.DeadlineExceeded",
			err:         fmt.Errorf("timeout: %w", context.DeadlineExceeded),
			wantCode:    connect.CodeDeadlineExceeded,
			wantMessage: "timeout: context deadline exceeded",
		},
		{
			name:        "ConnectCoder NotFound",
			err:         &codedError{msg: "user not found", code: connect.CodeNotFound},
			wantCode:    connect.CodeNotFound,
			wantMessage: "user not found",
		},
		{
			name:        "ConnectCoder InvalidArgument",
			err:         &codedError{msg: "invalid email", code: connect.CodeInvalidArgument},
			wantCode:    connect.CodeInvalidArgument,
			wantMessage: "invalid email",
		},
		{
			name:        "wrapped ConnectCoder",
			err:         fmt.Errorf("validation: %w", &codedError{msg: "bad input", code: connect.CodeInvalidArgument}),
			wantCode:    connect.CodeInvalidArgument,
			wantMessage: "validation: bad input",
		},
		{
			name:        "existing connect.Error",
			err:         connect.NewError(connect.CodePermissionDenied, "access denied"),
			wantCode:    connect.CodePermissionDenied,
			wantMessage: "access denied",
		},
		{
			name:        "wrapped connect.Error",
			err:         fmt.Errorf("auth check: %w", connect.NewError(connect.CodeUnauthenticated, "no token")),
			wantCode:    connect.CodeUnauthenticated,
			wantMessage: "no token",
		},
		{
			name:        "unmapped error",
			err:         errors.New("database connection failed"),
			wantCode:    connect.CodeInternal,
			wantMessage: "internal error",
		},
		{
			name:        "unmapped wrapped error",
			err:         fmt.Errorf("repository: %w", errors.New("sql error")),
			wantCode:    connect.CodeInternal,
			wantMessage: "internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := mapError(tt.err)

			if result.Code() != tt.wantCode {
				t.Errorf("code = %v, want %v", result.Code(), tt.wantCode)
			}
			if result.Message() != tt.wantMessage {
				t.Errorf("message = %q, want %q", result.Message(), tt.wantMessage)
			}
			// A preserved *connect.Error is part of tt.err's chain; any
			// other mapped error must keep tt.err as its cause.
			if !errors.Is(result, tt.err) && !errors.Is(tt.err, result) {
				t.Errorf("mapped error %v is unrelated to %v", result, tt.err)
			}
		})
	}
}

func TestInterceptor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		streamType  connect.StreamType
		handlerErr  error
		wantCode    connect.Code
		wantMessage string
	}{
		{
			name:        "unary ConnectCoder error",
			handlerErr:  &codedError{msg: "not found", code: connect.CodeNotFound},
			wantCode:    connect.CodeNotFound,
			wantMessage: "not found",
		},
		{
			name:        "unary context canceled",
			handlerErr:  context.Canceled,
			wantCode:    connect.CodeCanceled,
			wantMessage: "context canceled",
		},
		{
			name:        "unary unmapped error",
			handlerErr:  errors.New("internal failure"),
			wantCode:    connect.CodeInternal,
			wantMessage: "internal error",
		},
		{
			name:        "streaming ConnectCoder error",
			streamType:  connect.StreamTypeBidi,
			handlerErr:  &codedError{msg: "already exists", code: connect.CodeAlreadyExists},
			wantCode:    connect.CodeAlreadyExists,
			wantMessage: "already exists",
		},
		{
			name:        "streaming deadline exceeded",
			streamType:  connect.StreamTypeServer,
			handlerErr:  context.DeadlineExceeded,
			wantCode:    connect.CodeDeadlineExceeded,
			wantMessage: "context deadline exceeded",
		},
		{
			name:        "streaming unmapped error",
			streamType:  connect.StreamTypeClient,
			handlerErr:  errors.New("stream error"),
			wantCode:    connect.CodeInternal,
			wantMessage: "internal error",
		},
		{
			name: "no error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := connecttest.Serve(context.Background(), connecttest.Call{
				StreamType: tt.streamType,
				Handler: func(context.Context, connect.Spec, connect.ServerStream) error {
					return tt.handlerErr
				},
			}, NewInterceptor())

			if tt.handlerErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}

			var connectErr *connect.Error
			if !errors.As(err, &connectErr) {
				t.Fatalf("expected connect.Error, got %T", err)
			}
			if connectErr.Code() != tt.wantCode {
				t.Errorf("code = %v, want %v", connectErr.Code(), tt.wantCode)
			}
			if connectErr.Message() != tt.wantMessage {
				t.Errorf("message = %q, want %q", connectErr.Message(), tt.wantMessage)
			}
		})
	}
}
