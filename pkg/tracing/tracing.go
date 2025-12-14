// Package tracing provides utility functions for manual span creation.
package tracing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// WithSpan executes fn within a new span. Errors are automatically recorded.
func WithSpan(ctx context.Context, name string, fn func(context.Context) error) error {
	ctx, span := otel.Tracer("").Start(ctx, name)
	defer span.End()

	err := fn(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// WithSpanResult executes fn within a new span and returns the result.
// Errors are automatically recorded on the span.
func WithSpanResult[T any](ctx context.Context, name string, fn func(context.Context) (T, error)) (T, error) {
	ctx, span := otel.Tracer("").Start(ctx, name)
	defer span.End()

	result, err := fn(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return result, err
}
