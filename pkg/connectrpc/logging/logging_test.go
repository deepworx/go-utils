package logging

import (
	"context"
	"errors"
	"log/slog"
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

func TestLogRequest_Success(t *testing.T) {
	mock := &mockHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(mock))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	ctx := ctxutil.WithRequestID(context.Background(), "req-123")
	ctx = ctxutil.WithClaims(ctx, ctxutil.Claims{UserID: "user-456"})

	logRequest(ctx, "/test.Service/Method", nil)

	records := mock.getRecords()
	if len(records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(records))
	}

	record := records[0]
	if record.Level != slog.LevelInfo {
		t.Errorf("level = %v, want %v", record.Level, slog.LevelInfo)
	}
	if record.Message != "rpc completed" {
		t.Errorf("message = %q, want %q", record.Message, "rpc completed")
	}

	attrs := extractAttrs(record)
	if attrs["procedure"] != "/test.Service/Method" {
		t.Errorf("procedure = %q, want %q", attrs["procedure"], "/test.Service/Method")
	}
	if attrs["status"] != "ok" {
		t.Errorf("status = %q, want %q", attrs["status"], "ok")
	}
	if attrs["request_id"] != "req-123" {
		t.Errorf("request_id = %q, want %q", attrs["request_id"], "req-123")
	}
	if attrs["user_id"] != "user-456" {
		t.Errorf("user_id = %q, want %q", attrs["user_id"], "user-456")
	}
}

func TestLogRequest_Error(t *testing.T) {
	mock := &mockHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(mock))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	ctx := context.Background()
	err := connect.NewError(connect.CodeNotFound, errors.New("resource not found"))

	logRequest(ctx, "/test.Service/Get", err)

	records := mock.getRecords()
	if len(records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(records))
	}

	record := records[0]
	if record.Level != slog.LevelWarn {
		t.Errorf("level = %v, want %v", record.Level, slog.LevelWarn)
	}
	if record.Message != "rpc failed" {
		t.Errorf("message = %q, want %q", record.Message, "rpc failed")
	}

	attrs := extractAttrs(record)
	if attrs["status"] != "not_found" {
		t.Errorf("status = %q, want %q", attrs["status"], "not_found")
	}
	if attrs["error"] == "" {
		t.Error("error attribute should be present")
	}
}

func TestLogRequest_UnknownError(t *testing.T) {
	mock := &mockHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(mock))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	ctx := context.Background()
	err := errors.New("plain error")

	logRequest(ctx, "/test.Service/Method", err)

	records := mock.getRecords()
	if len(records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(records))
	}

	attrs := extractAttrs(records[0])
	if attrs["status"] != "unknown" {
		t.Errorf("status = %q, want %q", attrs["status"], "unknown")
	}
}

func TestInterceptor_WrapUnary(t *testing.T) {
	mock := &mockHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(mock))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	interceptor := NewInterceptor()
	wrapped := interceptor.WrapUnary(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return &mockResponse{}, nil
	})

	ctx := ctxutil.WithRequestID(context.Background(), "unary-req")
	req := &mockRequest{procedure: "/test.Service/Unary"}

	resp, err := wrapped(ctx, req)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Error("expected response")
	}

	records := mock.getRecords()
	if len(records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(records))
	}
	if records[0].Level != slog.LevelInfo {
		t.Errorf("level = %v, want %v", records[0].Level, slog.LevelInfo)
	}
}

func TestInterceptor_WrapStreamingHandler(t *testing.T) {
	mock := &mockHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(mock))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	interceptor := NewInterceptor()
	wrapped := interceptor.WrapStreamingHandler(func(_ context.Context, _ connect.StreamingHandlerConn) error {
		return nil
	})

	ctx := context.Background()
	conn := &mockStreamingConn{procedure: "/test.Service/Stream"}

	err := wrapped(ctx, conn)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
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

func TestGetStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "ok",
		},
		{
			name: "connect error",
			err:  connect.NewError(connect.CodeInvalidArgument, errors.New("bad request")),
			want: "invalid_argument",
		},
		{
			name: "plain error",
			err:  errors.New("something went wrong"),
			want: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := getStatus(tt.err)
			if got != tt.want {
				t.Errorf("getStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func extractAttrs(r slog.Record) map[string]string {
	attrs := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
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
