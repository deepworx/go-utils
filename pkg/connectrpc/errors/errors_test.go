package errors

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
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
			err:         connect.NewError(connect.CodePermissionDenied, errors.New("access denied")),
			wantCode:    connect.CodePermissionDenied,
			wantMessage: "access denied",
		},
		{
			name:        "wrapped connect.Error",
			err:         fmt.Errorf("auth check: %w", connect.NewError(connect.CodeUnauthenticated, errors.New("no token"))),
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
		})
	}
}

func TestInterceptor_WrapUnary_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		handlerErr  error
		wantCode    connect.Code
		wantMessage string
	}{
		{
			name:        "ConnectCoder error",
			handlerErr:  &codedError{msg: "not found", code: connect.CodeNotFound},
			wantCode:    connect.CodeNotFound,
			wantMessage: "not found",
		},
		{
			name:        "context canceled",
			handlerErr:  context.Canceled,
			wantCode:    connect.CodeCanceled,
			wantMessage: "context canceled",
		},
		{
			name:        "unmapped error",
			handlerErr:  errors.New("internal failure"),
			wantCode:    connect.CodeInternal,
			wantMessage: "internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			interceptor := NewInterceptor()
			wrapped := interceptor.WrapUnary(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
				return nil, tt.handlerErr
			})

			req := &mockRequest{procedure: "/test.Service/Method"}
			_, err := wrapped(context.Background(), req)

			if err == nil {
				t.Fatal("expected error, got nil")
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

func TestInterceptor_WrapUnary_NoError(t *testing.T) {
	t.Parallel()

	interceptor := NewInterceptor()
	wrapped := interceptor.WrapUnary(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return &mockResponse{}, nil
	})

	req := &mockRequest{procedure: "/test.Service/Method"}
	resp, err := wrapped(context.Background(), req)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Error("expected response, got nil")
	}
}

func TestInterceptor_WrapStreamingHandler_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		handlerErr  error
		wantCode    connect.Code
		wantMessage string
	}{
		{
			name:        "ConnectCoder error",
			handlerErr:  &codedError{msg: "already exists", code: connect.CodeAlreadyExists},
			wantCode:    connect.CodeAlreadyExists,
			wantMessage: "already exists",
		},
		{
			name:        "deadline exceeded",
			handlerErr:  context.DeadlineExceeded,
			wantCode:    connect.CodeDeadlineExceeded,
			wantMessage: "context deadline exceeded",
		},
		{
			name:        "unmapped error",
			handlerErr:  errors.New("stream error"),
			wantCode:    connect.CodeInternal,
			wantMessage: "internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			interceptor := NewInterceptor()
			wrapped := interceptor.WrapStreamingHandler(func(_ context.Context, _ connect.StreamingHandlerConn) error {
				return tt.handlerErr
			})

			conn := &mockStreamingConn{procedure: "/test.Service/Stream"}
			err := wrapped(context.Background(), conn)

			if err == nil {
				t.Fatal("expected error, got nil")
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

func TestInterceptor_WrapStreamingHandler_NoError(t *testing.T) {
	t.Parallel()

	interceptor := NewInterceptor()
	wrapped := interceptor.WrapStreamingHandler(func(_ context.Context, _ connect.StreamingHandlerConn) error {
		return nil
	})

	conn := &mockStreamingConn{procedure: "/test.Service/Stream"}
	err := wrapped(context.Background(), conn)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestInterceptor_WrapStreamingClient_PassThrough(t *testing.T) {
	t.Parallel()

	interceptor := NewInterceptor()
	called := false
	original := func(_ context.Context, _ connect.Spec) connect.StreamingClientConn {
		called = true
		return nil
	}

	wrapped := interceptor.WrapStreamingClient(original)
	wrapped(context.Background(), connect.Spec{})

	if !called {
		t.Error("expected original function to be called")
	}
}

type mockRequest struct {
	connect.AnyRequest
	procedure string
}

func (r *mockRequest) Spec() connect.Spec {
	return connect.Spec{Procedure: r.procedure}
}

type mockResponse struct {
	connect.AnyResponse
}

type mockStreamingConn struct {
	connect.StreamingHandlerConn
	procedure string
}

func (c *mockStreamingConn) Spec() connect.Spec {
	return connect.Spec{Procedure: c.procedure}
}
