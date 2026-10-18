// Package otel provides OpenTelemetry initialization for tracing, metrics, and logging.
package otel

import (
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"

	"github.com/deepworx/go-utils/pkg/shutdown"
	"github.com/deepworx/go-utils/pkg/slogutil"
	"github.com/google/uuid"
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

	// Histograms selects the aggregation of all histogram instruments.
	// Valid values: "exponential", "explicit".
	// Default: "exponential"
	Histograms Histograms `koanf:"histograms"`
}

// Histograms is the aggregation used for histogram instruments.
type Histograms string

const (
	// HistogramsExponential aggregates histograms as base-2 exponential
	// histograms, which Prometheus-compatible backends ingest as native
	// histograms.
	HistogramsExponential Histograms = "exponential"

	// HistogramsExplicit aggregates histograms into the explicit bucket
	// boundaries advised by each instrument.
	HistogramsExplicit Histograms = "explicit"
)

// ErrInvalidHistograms is returned by Setup when Config.Histograms holds an
// unknown value.
var ErrInvalidHistograms = errors.New("invalid histograms aggregation")

// Exponential histogram limits, matching the OpenTelemetry SDK defaults.
const (
	exponentialMaxSize  = 160
	exponentialMaxScale = 20
)

// serviceInstanceID identifies this process. OTEL_RESOURCE_ATTRIBUTES can
// override it, e.g. with the pod name.
var serviceInstanceID = uuid.NewString()

// DefaultConfig returns a Config with default values.
// ServiceName and ServiceVersion are required and must be set by the caller.
func DefaultConfig() Config {
	return Config{Histograms: HistogramsExponential}
}

// Setup initializes OpenTelemetry providers and registers shutdown handlers.
// Exporters are configured via OTEL_* environment variables.
//
// Setup wraps the handler of slog.Default() in a slogutil.ContextHandler,
// unless it already is one, so records logged with a context carry its
// correlation fields. It returns ErrInvalidHistograms if cfg.Histograms is
// unknown.
func Setup(ctx context.Context, cfg Config) error {
	if err := cfg.Histograms.validate(); err != nil {
		return err
	}

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

	reader, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		return err
	}
	mp := newMeterProvider(meterProviderInput{res: res, reader: reader, histograms: cfg.Histograms})
	otel.SetMeterProvider(mp)

	lp, err := newLoggerProvider(ctx, res)
	if err != nil {
		return err
	}

	installContextHandler()

	shutdown.Register(func(ctx context.Context) error {
		return errors.Join(
			tp.Shutdown(ctx),
			mp.Shutdown(ctx),
			lp.Shutdown(ctx),
		)
	})

	return nil
}

func (h Histograms) validate() error {
	switch h {
	case "", HistogramsExponential, HistogramsExplicit:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidHistograms, h)
	}
}

func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithAttributes(semconv.ServiceInstanceID(serviceInstanceID)),
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

// meterProviderInput is what a MeterProvider is built from.
type meterProviderInput struct {
	res        *resource.Resource
	reader     metric.Reader
	histograms Histograms
}

func newMeterProvider(in meterProviderInput) *metric.MeterProvider {
	opts := []metric.Option{
		metric.WithResource(in.res),
		metric.WithReader(in.reader),
	}
	if in.histograms != HistogramsExplicit {
		opts = append(opts, metric.WithView(metric.NewView(
			metric.Instrument{Kind: metric.InstrumentKindHistogram},
			metric.Stream{Aggregation: metric.AggregationBase2ExponentialHistogram{
				MaxSize:  exponentialMaxSize,
				MaxScale: exponentialMaxScale,
			}},
		)))
	}
	return metric.NewMeterProvider(opts...)
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

// installContextHandler wraps the default slog handler in a
// slogutil.ContextHandler. slog.SetDefault redirects the log package into the
// new handler; restoring the previous log output keeps log.Print working and
// avoids a deadlock when the wrapped handler is slog's builtin one, which
// writes through the log package itself.
func installContextHandler() {
	handler := slog.Default().Handler()
	if _, ok := handler.(*slogutil.ContextHandler); ok {
		return
	}
	writer, flags := stdlog.Writer(), stdlog.Flags()
	slog.SetDefault(slog.New(slogutil.NewContextHandler(handler)))
	stdlog.SetOutput(writer)
	stdlog.SetFlags(flags)
}
