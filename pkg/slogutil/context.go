package slogutil

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/pkg/ctxutil"
)

// ContextHandler is a slog.Handler that adds request_id, tenant_id, user_id,
// correlation_id, trace_id and span_id from the record's context before
// passing the record to the wrapped handler. Fields whose value is empty
// or missing are omitted. trace_id and span_id come from the active
// OpenTelemetry span context.
//
// Fields are added to the record, so they are nested under any group
// opened with WithGroup.
type ContextHandler struct {
	next slog.Handler
}

// NewContextHandler returns a ContextHandler that wraps next.
func NewContextHandler(next slog.Handler) *ContextHandler {
	return &ContextHandler{next: next}
}

// Enabled reports whether the wrapped handler handles records at level.
func (h *ContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle adds the context fields to r and passes it to the wrapped handler.
func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := contextAttrs(ctx); len(attrs) > 0 {
		r = r.Clone()
		r.AddAttrs(attrs...)
	}
	return h.next.Handle(ctx, r)
}

// WithAttrs returns a ContextHandler whose wrapped handler has attrs added.
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{next: h.next.WithAttrs(attrs)}
}

// WithGroup returns a ContextHandler whose wrapped handler opens group name.
func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{next: h.next.WithGroup(name)}
}

func contextAttrs(ctx context.Context) []slog.Attr {
	var attrs []slog.Attr
	add := func(key, value string, ok bool) {
		if ok && value != "" {
			attrs = append(attrs, slog.String(key, value))
		}
	}

	requestID, ok := ctxutil.RequestID(ctx)
	add("request_id", requestID, ok)
	tenantID, ok := ctxutil.TenantID(ctx)
	add("tenant_id", tenantID, ok)
	userID, ok := ctxutil.UserID(ctx)
	add("user_id", userID, ok)
	correlationID, ok := ctxutil.CorrelationID(ctx)
	add("correlation_id", correlationID, ok)

	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs,
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return attrs
}
