// Package otel provides OpenTelemetry initialization for tracing, metrics, and logging.
package otel

import (
	"context"
	"errors"

	"github.com/deepworx/go-utils/pkg/shutdown"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config holds the configuration for OpenTelemetry setup.
type Config struct {
	// ServiceName is the name of the service. Required.
	ServiceName string `koanf:"service_name"`

	// ServiceVersion is the version of the service. Required.
	ServiceVersion string `koanf:"service_version"`
}

// DefaultConfig returns a Config with default values.
// ServiceName and ServiceVersion are required and must be set by the caller.
func DefaultConfig() Config {
	return Config{}
}

// Setup initializes OpenTelemetry providers and registers shutdown handlers.
// Exporters are configured via OTEL_* environment variables.
func Setup(ctx context.Context, cfg Config) error {
	res, err := newResource(ctx, cfg)
	if err != nil {
		return err
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	tp, err := newTracerProvider(ctx, res)
	if err != nil {
		return err
	}
	otel.SetTracerProvider(tp)

	mp, err := newMeterProvider(ctx, res)
	if err != nil {
		return err
	}
	otel.SetMeterProvider(mp)

	lp, err := newLoggerProvider(ctx, res)
	if err != nil {
		return err
	}

	shutdown.Register(func(ctx context.Context) error {
		return errors.Join(
			tp.Shutdown(ctx),
			mp.Shutdown(ctx),
			lp.Shutdown(ctx),
		)
	})

	return nil
}

func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithHost(),
		resource.WithOS(),
		resource.WithContainer(),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
}

func newTracerProvider(ctx context.Context, res *resource.Resource) (*trace.TracerProvider, error) {
	exp, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return nil, err
	}
	return trace.NewTracerProvider(
		trace.WithResource(res),
		trace.WithBatcher(exp),
	), nil
}

func newMeterProvider(ctx context.Context, res *resource.Resource) (*metric.MeterProvider, error) {
	reader, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		return nil, err
	}
	return metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(reader),
	), nil
}

func newLoggerProvider(ctx context.Context, res *resource.Resource) (*log.LoggerProvider, error) {
	exp, err := autoexport.NewLogExporter(ctx)
	if err != nil {
		return nil, err
	}
	return log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(log.NewBatchProcessor(exp)),
	), nil
}
