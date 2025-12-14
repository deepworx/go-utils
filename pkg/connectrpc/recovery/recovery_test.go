package recovery

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"github.com/deepworx/go-utils/pkg/ctxutil"
)

type mockHandler struct {
	records []slog.Record
	mu      sync.Mutex
}

func (h *mockHandler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

func (h *mockHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *mockHandler) WithAttrs(_ []slog.Attr) slog.Handler {
	return h
}

func (h *mockHandler) WithGroup(_ string) slog.Handler {
	return h
}

func (h *mockHandler) getRecords() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.records
}

func TestRecoverPanic(t *testing.T) {
	tests := []struct {
		name       string
		panicValue any
		procedure  string
		requestID  string
		wantPanic  string
	}{
		{
			name:       "panic with string",
			panicValue: "something went wrong",
			procedure:  "/test.Service/Method",
			requestID:  "req-123",
			wantPanic:  "something went wrong",
		},
		{
			name:       "panic with error",
			panicValue: errors.New("error panic"),
			procedure:  "/test.Service/Error",
			requestID:  "req-456",
			wantPanic:  "error panic",
		},
		{
			name:       "panic with int",
			panicValue: 42,
			procedure:  "/test.Service/Int",
			requestID:  "",
			wantPanic:  "42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockHandler{}
			oldLogger := slog.Default()
			slog.SetDefault(slog.New(mock))
			t.Cleanup(func() { slog.SetDefault(oldLogger) })

			ctx := context.Background()
			if tt.requestID != "" {
				ctx = ctxutil.WithRequestID(ctx, tt.requestID)
			}

			err := recoverPanic(ctx, tt.procedure, tt.panicValue)

			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if err.Code() != connect.CodeInternal {
				t.Errorf("code = %v, want %v", err.Code(), connect.CodeInternal)
			}
			if err.Message() != "internal error" {
				t.Errorf("message = %q, want %q", err.Message(), "internal error")
			}

			records := mock.getRecords()
			if len(records) != 1 {
				t.Fatalf("expected 1 log record, got %d", len(records))
			}

			record := records[0]
			if record.Level != slog.LevelError {
				t.Errorf("level = %v, want %v", record.Level, slog.LevelError)
			}
			if record.Message != "panic recovered" {
				t.Errorf("message = %q, want %q", record.Message, "panic recovered")
			}

			attrs := extractAttrs(record)
			if attrs["procedure"] != tt.procedure {
				t.Errorf("procedure = %q, want %q", attrs["procedure"], tt.procedure)
			}

			panicStr, _ := attrs["panic"].(string)
			if !strings.Contains(panicStr, tt.wantPanic) {
				t.Errorf("panic = %q, want to contain %q", panicStr, tt.wantPanic)
			}

			stackStr, ok := attrs["stack"].(string)
			if !ok || stackStr == "" {
				t.Error("stack trace not found or empty")
			}
			if !strings.Contains(stackStr, "recoverPanic") {
				t.Error("stack trace should contain recoverPanic function")
			}

			if tt.requestID != "" {
				if attrs["request_id"] != tt.requestID {
					t.Errorf("request_id = %q, want %q", attrs["request_id"], tt.requestID)
				}
			}
		})
	}
}

func TestInterceptor_WrapUnary_Panic(t *testing.T) {
	mock := &mockHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(mock))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	interceptor := NewInterceptor()
	wrapped := interceptor.WrapUnary(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		panic("handler panic")
	})

	ctx := ctxutil.WithRequestID(context.Background(), "unary-req-1")
	req := &mockRequest{procedure: "/test.Service/Unary"}

	resp, err := wrapped(ctx, req)

	if resp != nil {
		t.Error("expected nil response")
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("expected connect.Error, got %T", err)
	}
	if connectErr.Code() != connect.CodeInternal {
		t.Errorf("code = %v, want %v", connectErr.Code(), connect.CodeInternal)
	}

	records := mock.getRecords()
	if len(records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(records))
	}
}

func TestInterceptor_WrapUnary_NoPanic(t *testing.T) {
	mock := &mockHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(mock))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	interceptor := NewInterceptor()
	wrapped := interceptor.WrapUnary(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return &mockResponse{}, nil
	})

	ctx := context.Background()
	req := &mockRequest{procedure: "/test.Service/Normal"}

	resp, err := wrapped(ctx, req)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Error("expected response, got nil")
	}

	records := mock.getRecords()
	if len(records) != 0 {
		t.Errorf("expected 0 log records, got %d", len(records))
	}
}

func TestInterceptor_WrapStreamingHandler_Panic(t *testing.T) {
	mock := &mockHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(mock))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	interceptor := NewInterceptor()
	wrapped := interceptor.WrapStreamingHandler(func(_ context.Context, _ connect.StreamingHandlerConn) error {
		panic("streaming panic")
	})

	ctx := ctxutil.WithRequestID(context.Background(), "stream-req-1")
	conn := &mockStreamingConn{procedure: "/test.Service/Stream"}

	err := wrapped(ctx, conn)

	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("expected connect.Error, got %T", err)
	}
	if connectErr.Code() != connect.CodeInternal {
		t.Errorf("code = %v, want %v", connectErr.Code(), connect.CodeInternal)
	}

	records := mock.getRecords()
	if len(records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(records))
	}

	attrs := extractAttrs(records[0])
	if attrs["procedure"] != "/test.Service/Stream" {
		t.Errorf("procedure = %q, want %q", attrs["procedure"], "/test.Service/Stream")
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

func extractAttrs(r slog.Record) map[string]any {
	attrs := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		switch v := a.Value.Any().(type) {
		case string:
			attrs[a.Key] = v
		case error:
			attrs[a.Key] = v.Error()
		default:
			attrs[a.Key] = a.Value.String()
		}
		return true
	})
	return attrs
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
