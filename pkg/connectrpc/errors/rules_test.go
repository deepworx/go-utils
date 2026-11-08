package errors

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/deepworx/go-utils/internal/connecttest"
)

var errMissing = errors.New("missing")

type invalidFieldError struct{ field string }

func (e *invalidFieldError) Error() string { return "invalid " + e.field }

func TestMapRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		err         error
		opts        []Option
		wantCode    connect.Code
		wantMessage string
	}{
		{
			name:        "Is rule matches wrapped sentinel",
			err:         fmt.Errorf("load: %w", errMissing),
			opts:        []Option{WithRules(Is(errMissing, connect.CodeNotFound))},
			wantCode:    connect.CodeNotFound,
			wantMessage: "load: missing",
		},
		{
			name:        "As rule matches wrapped type",
			err:         fmt.Errorf("decode: %w", &invalidFieldError{field: "email"}),
			opts:        []Option{WithRules(As[*invalidFieldError](connect.CodeInvalidArgument))},
			wantCode:    connect.CodeInvalidArgument,
			wantMessage: "decode: invalid email",
		},
		{
			name: "first matching rule wins",
			err:  errMissing,
			opts: []Option{
				WithRules(Is(errMissing, connect.CodeNotFound)),
				WithRules(Is(errMissing, connect.CodeFailedPrecondition)),
			},
			wantCode:    connect.CodeNotFound,
			wantMessage: "missing",
		},
		{
			name:        "non-matching rule is skipped",
			err:         errMissing,
			opts:        []Option{WithRules(Rule{}, As[*invalidFieldError](connect.CodeInvalidArgument), Is(errMissing, connect.CodeNotFound))},
			wantCode:    connect.CodeNotFound,
			wantMessage: "missing",
		},
		{
			name:        "context error beats rule",
			err:         fmt.Errorf("%w: %w", errMissing, context.Canceled),
			opts:        []Option{WithRules(Is(errMissing, connect.CodeNotFound))},
			wantCode:    connect.CodeCanceled,
			wantMessage: "missing: context canceled",
		},
		{
			name:        "rule beats ConnectCoder",
			err:         fmt.Errorf("%w: %w", errMissing, &codedError{msg: "coded", code: connect.CodeAborted}),
			opts:        []Option{WithRules(Is(errMissing, connect.CodeNotFound))},
			wantCode:    connect.CodeNotFound,
			wantMessage: "missing: coded",
		},
		{
			name:        "ConnectCoder applies when no rule matches",
			err:         &codedError{msg: "coded", code: connect.CodeAborted},
			opts:        []Option{WithRules(Is(errMissing, connect.CodeNotFound))},
			wantCode:    connect.CodeAborted,
			wantMessage: "coded",
		},
		{
			name:        "connect.Error is preserved when no rule matches",
			err:         connect.NewError(connect.CodePermissionDenied, "denied"),
			opts:        []Option{WithRules(Is(errMissing, connect.CodeNotFound))},
			wantCode:    connect.CodePermissionDenied,
			wantMessage: "denied",
		},
		{
			name:        "unmapped error is sanitized",
			err:         errors.New("db down"),
			opts:        []Option{WithRules(Is(errMissing, connect.CodeNotFound))},
			wantCode:    connect.CodeInternal,
			wantMessage: "internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Map(tt.err, tt.opts...)

			if got.Code() != tt.wantCode {
				t.Errorf("code = %v, want %v", got.Code(), tt.wantCode)
			}
			if got.Message() != tt.wantMessage {
				t.Errorf("message = %q, want %q", got.Message(), tt.wantMessage)
			}
			if !errors.Is(got, tt.err) {
				t.Errorf("cause of %v is not %v", got, tt.err)
			}
		})
	}
}

func TestMapNil(t *testing.T) {
	t.Parallel()

	if got := Map(nil, WithRules(Is(errMissing, connect.CodeNotFound))); got != nil {
		t.Errorf("Map(nil) = %v, want nil", got)
	}
}

func TestInterceptorRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		streamType  connect.StreamType
		handlerErr  error
		wantCode    connect.Code
		wantMessage string
		wantDetails int
	}{
		{
			name:        "unary rule with reason",
			handlerErr:  fmt.Errorf("load: %w", errMissing),
			wantCode:    connect.CodeNotFound,
			wantMessage: "load: missing",
			wantDetails: 1,
		},
		{
			name:        "streaming rule without reason",
			streamType:  connect.StreamTypeServer,
			handlerErr:  &invalidFieldError{field: "email"},
			wantCode:    connect.CodeInvalidArgument,
			wantMessage: "invalid email",
		},
		{
			name:        "unmapped error",
			handlerErr:  errors.New("db down"),
			wantCode:    connect.CodeInternal,
			wantMessage: "internal error",
		},
	}

	interceptor := NewInterceptor(
		WithDomain("tasks.example.com"),
		WithRules(
			Is(errMissing, connect.CodeNotFound).WithReason("NOT_FOUND"),
			As[*invalidFieldError](connect.CodeInvalidArgument),
		),
	)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := connecttest.Serve(context.Background(), connecttest.Call{
				StreamType: tt.streamType,
				Handler: func(context.Context, connect.Spec, connect.ServerStream) error {
					return tt.handlerErr
				},
			}, interceptor)

			got, ok := errors.AsType[*connect.Error](err)
			if !ok {
				t.Fatalf("expected *connect.Error, got %T", err)
			}
			if got.Code() != tt.wantCode {
				t.Errorf("code = %v, want %v", got.Code(), tt.wantCode)
			}
			if got.Message() != tt.wantMessage {
				t.Errorf("message = %q, want %q", got.Message(), tt.wantMessage)
			}
			if n := len(got.Details()); n != tt.wantDetails {
				t.Errorf("got %d details, want %d", n, tt.wantDetails)
			}
		})
	}
}
