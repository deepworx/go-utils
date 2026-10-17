package slogutil_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/slogutil"
)

const (
	testTraceID = "0102030405060708090a0b0c0d0e0f10"
	testSpanID  = "0102030405060708"
)

func withSpan(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	traceID, err := trace.TraceIDFromHex(testTraceID)
	if err != nil {
		t.Fatalf("parse trace id: %v", err)
	}
	spanID, err := trace.SpanIDFromHex(testSpanID)
	if err != nil {
		t.Fatalf("parse span id: %v", err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(ctx, sc)
}

func decodeLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode log line %q: %v", buf.String(), err)
	}
	return got
}

func TestContextHandler_Fields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		ctx   func(t *testing.T) context.Context
		key   string
		value string
	}{
		{
			name: "request_id present",
			ctx: func(*testing.T) context.Context {
				return ctxutil.WithRequestID(context.Background(), "req-1")
			},
			key:   "request_id",
			value: "req-1",
		},
		{
			name: "request_id empty",
			ctx: func(*testing.T) context.Context {
				return ctxutil.WithRequestID(context.Background(), "")
			},
			key: "request_id",
		},
		{
			name: "tenant_id present",
			ctx: func(*testing.T) context.Context {
				return ctxutil.WithTenantID(context.Background(), "tenant-1")
			},
			key:   "tenant_id",
			value: "tenant-1",
		},
		{
			name: "tenant_id from claims",
			ctx: func(*testing.T) context.Context {
				return ctxutil.WithClaims(context.Background(), ctxutil.Claims{TenantID: "tenant-2"})
			},
			key:   "tenant_id",
			value: "tenant-2",
		},
		{
			name: "tenant_id empty",
			ctx: func(*testing.T) context.Context {
				return ctxutil.WithTenantID(context.Background(), "")
			},
			key: "tenant_id",
		},
		{
			name: "user_id present",
			ctx: func(*testing.T) context.Context {
				return ctxutil.WithClaims(context.Background(), ctxutil.Claims{UserID: "user-1"})
			},
			key:   "user_id",
			value: "user-1",
		},
		{
			name: "user_id empty",
			ctx: func(*testing.T) context.Context {
				return ctxutil.WithClaims(context.Background(), ctxutil.Claims{UserID: ""})
			},
			key: "user_id",
		},
		{
			name: "correlation_id present",
			ctx: func(*testing.T) context.Context {
				return ctxutil.WithCorrelationID(context.Background(), "corr-1")
			},
			key:   "correlation_id",
			value: "corr-1",
		},
		{
			name: "correlation_id empty",
			ctx: func(*testing.T) context.Context {
				return ctxutil.WithCorrelationID(context.Background(), "")
			},
			key: "correlation_id",
		},
		{
			name:  "trace_id from span",
			ctx:   func(t *testing.T) context.Context { return withSpan(t, context.Background()) },
			key:   "trace_id",
			value: testTraceID,
		},
		{
			name:  "span_id from span",
			ctx:   func(t *testing.T) context.Context { return withSpan(t, context.Background()) },
			key:   "span_id",
			value: testSpanID,
		},
		{
			name: "trace_id without span",
			ctx:  func(*testing.T) context.Context { return context.Background() },
			key:  "trace_id",
		},
		{
			name: "span_id without span",
			ctx:  func(*testing.T) context.Context { return context.Background() },
			key:  "span_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := slog.New(slogutil.NewContextHandler(slog.NewJSONHandler(&buf, nil)))
			logger.InfoContext(tt.ctx(t), "msg")
			got := decodeLine(t, &buf)

			value, present := got[tt.key]
			if tt.value == "" {
				if present {
					t.Errorf("%s = %v, want absent", tt.key, value)
				}
				return
			}
			if value != tt.value {
				t.Errorf("%s = %v, want %q", tt.key, value, tt.value)
			}
		})
	}
}

func TestContextHandler_KeepsOtherAttrs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slogutil.NewContextHandler(slog.NewJSONHandler(&buf, nil))).With("service", "svc")
	logger.InfoContext(ctxutil.WithRequestID(context.Background(), "req-1"), "msg", "k", "v")
	got := decodeLine(t, &buf)

	want := map[string]string{"service": "svc", "k": "v", "request_id": "req-1"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %q", k, got[k], v)
		}
	}
}

func TestContextHandler_Enabled(t *testing.T) {
	t.Parallel()

	h := slogutil.NewContextHandler(slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled(info) = true, want false")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled(error) = false, want true")
	}
}
