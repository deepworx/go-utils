package recovery

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/deepworx/go-utils/internal/connecttest"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/slogutil"
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
			slog.SetDefault(slog.New(slogutil.NewContextHandler(mock)))
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
				if n := countAttr(record, "request_id"); n != 1 {
					t.Errorf("request_id appears %d times, want 1", n)
				}
			}
		})
	}
}

func TestInterceptor(t *testing.T) {
	tests := []struct {
		name       string
		streamType connect.StreamType
		handler    connect.ServerFunc
		wantCode   connect.Code
		wantLogs   int
	}{
		{
			name: "unary panic",
			handler: func(context.Context, connect.Spec, connect.ServerStream) error {
				panic("handler panic")
			},
			wantCode: connect.CodeInternal,
			wantLogs: 1,
		},
		{
			name:       "streaming panic",
			streamType: connect.StreamTypeBidi,
			handler: func(context.Context, connect.Spec, connect.ServerStream) error {
				panic("streaming panic")
			},
			wantCode: connect.CodeInternal,
			wantLogs: 1,
		},
		{
			name: "no panic",
			handler: func(context.Context, connect.Spec, connect.ServerStream) error {
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockHandler{}
			oldLogger := slog.Default()
			slog.SetDefault(slog.New(slogutil.NewContextHandler(mock)))
			t.Cleanup(func() { slog.SetDefault(oldLogger) })

			err := connecttest.Serve(context.Background(), connecttest.Call{
				StreamType: tt.streamType,
				Handler:    tt.handler,
			}, NewInterceptor())

			if tt.wantCode == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				var connectErr *connect.Error
				if !errors.As(err, &connectErr) {
					t.Fatalf("expected connect.Error, got %T", err)
				}
				if connectErr.Code() != tt.wantCode {
					t.Errorf("code = %v, want %v", connectErr.Code(), tt.wantCode)
				}
			}

			records := mock.getRecords()
			if len(records) != tt.wantLogs {
				t.Fatalf("expected %d log records, got %d", tt.wantLogs, len(records))
			}
			if tt.wantLogs > 0 {
				attrs := extractAttrs(records[0])
				if attrs["procedure"] != connecttest.Procedure {
					t.Errorf("procedure = %q, want %q", attrs["procedure"], connecttest.Procedure)
				}
			}
		})
	}
}

func countAttr(r slog.Record, key string) int {
	n := 0
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			n++
		}
		return true
	})
	return n
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
