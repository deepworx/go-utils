package otel

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
)

func TestSetup(t *testing.T) {
	// Use "none" exporters to avoid needing an OTLP endpoint
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")

	ctx := context.Background()
	err := Setup(ctx, Config{
		ServiceName:    "test-service",
		ServiceVersion: "1.0.0",
	})
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}

	// Verify tracer provider is set
	tp := otel.GetTracerProvider()
	if tp == nil {
		t.Error("TracerProvider not set")
	}

	// Verify meter provider is set
	mp := otel.GetMeterProvider()
	if mp == nil {
		t.Error("MeterProvider not set")
	}
}

func TestNewResource(t *testing.T) {
	ctx := context.Background()
	res, err := newResource(ctx, Config{
		ServiceName:    "test-service",
		ServiceVersion: "1.0.0",
	})
	if err != nil {
		t.Fatalf("newResource() error = %v", err)
	}
	if res == nil {
		t.Error("newResource() returned nil")
	}
}
