package otel

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"strings"
	"testing"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/slogutil"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func TestSetup(t *testing.T) {
	// Use "none" exporters to avoid needing an OTLP endpoint
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	restoreDefaultLogger(t)

	ctx := context.Background()
	err := Setup(ctx, Config{
		ServiceName:    "test-service",
		ServiceVersion: "1.0.0",
	})
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}

	if otel.GetTracerProvider() == nil {
		t.Error("TracerProvider not set")
	}
	if otel.GetMeterProvider() == nil {
		t.Error("MeterProvider not set")
	}
}

func TestSetup_InvalidHistograms(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	restoreDefaultLogger(t)

	err := Setup(context.Background(), Config{
		ServiceName:    "test-service",
		ServiceVersion: "1.0.0",
		Histograms:     "linear",
	})
	if !errors.Is(err, ErrInvalidHistograms) {
		t.Fatalf("Setup() error = %v, want %v", err, ErrInvalidHistograms)
	}
}

func TestMeterProvider_HistogramAggregation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		histograms Histograms
		want       string
	}{
		{name: "default is exponential", histograms: "", want: "exponential"},
		{name: "exponential", histograms: HistogramsExponential, want: "exponential"},
		{name: "explicit", histograms: HistogramsExplicit, want: "explicit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := sdkmetric.NewManualReader()
			mp := newMeterProvider(resource.Empty(), reader, tt.histograms)
			t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

			ctx := context.Background()
			hist, err := mp.Meter("test").Float64Histogram("test.duration")
			if err != nil {
				t.Fatalf("Float64Histogram() error = %v", err)
			}
			hist.Record(ctx, 0.25)

			var rm metricdata.ResourceMetrics
			if err := reader.Collect(ctx, &rm); err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if got := aggregationKind(t, rm); got != tt.want {
				t.Errorf("aggregation = %s, want %s", got, tt.want)
			}
		})
	}
}

func aggregationKind(t *testing.T, rm metricdata.ResourceMetrics) string {
	t.Helper()
	if len(rm.ScopeMetrics) != 1 || len(rm.ScopeMetrics[0].Metrics) != 1 {
		t.Fatalf("collected %d scopes, want 1 scope with 1 metric", len(rm.ScopeMetrics))
	}
	switch rm.ScopeMetrics[0].Metrics[0].Data.(type) {
	case metricdata.ExponentialHistogram[float64]:
		return "exponential"
	case metricdata.Histogram[float64]:
		return "explicit"
	default:
		return "other"
	}
}

func TestNewResource_ServiceInstanceID(t *testing.T) {
	cfg := Config{ServiceName: "test-service", ServiceVersion: "1.0.0"}

	tests := []struct {
		name  string
		env   string
		check func(t *testing.T, first, second string)
	}{
		{
			name: "generated and stable for the process",
			check: func(t *testing.T, first, second string) {
				if first == "" {
					t.Fatal("service.instance.id is empty")
				}
				if first != second {
					t.Errorf("service.instance.id changed: %q then %q", first, second)
				}
			},
		},
		{
			name: "overridden by OTEL_RESOURCE_ATTRIBUTES",
			env:  "service.instance.id=pod-1",
			check: func(t *testing.T, first, _ string) {
				if first != "pod-1" {
					t.Errorf("service.instance.id = %q, want %q", first, "pod-1")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", tt.env)
			tt.check(t, instanceID(t, cfg), instanceID(t, cfg))
		})
	}
}

func instanceID(t *testing.T, cfg Config) string {
	t.Helper()
	res, err := newResource(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newResource() error = %v", err)
	}
	v, ok := res.Set().Value(semconv.ServiceInstanceIDKey)
	if !ok {
		t.Fatal("service.instance.id missing from resource")
	}
	return v.AsString()
}

func TestSetup_InstallsContextHandler(t *testing.T) {
	tests := []struct {
		name    string
		install func(buf *bytes.Buffer)
	}{
		{
			name: "wraps custom default handler",
			install: func(buf *bytes.Buffer) {
				slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
			},
		},
		{
			name: "wraps builtin default handler",
			install: func(buf *bytes.Buffer) {
				slog.SetDefault(builtinDefault)
				log.SetOutput(buf)
			},
		},
		{
			name: "keeps existing context handler",
			install: func(buf *bytes.Buffer) {
				slog.SetDefault(slog.New(slogutil.NewContextHandler(slog.NewTextHandler(buf, nil))))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_TRACES_EXPORTER", "none")
			t.Setenv("OTEL_METRICS_EXPORTER", "none")
			t.Setenv("OTEL_LOGS_EXPORTER", "none")
			restoreDefaultLogger(t)

			var buf bytes.Buffer
			tt.install(&buf)

			if err := Setup(context.Background(), Config{ServiceName: "svc", ServiceVersion: "1.0.0"}); err != nil {
				t.Fatalf("Setup() error = %v", err)
			}

			ctx := ctxutil.WithRequestID(context.Background(), "req-1")
			slog.InfoContext(ctx, "hello")
			log.Print("plain")

			out := buf.String()
			if got := strings.Count(out, "request_id=req-1"); got != 1 {
				t.Errorf("request_id occurrences = %d, want 1; output:\n%s", got, out)
			}
			if !strings.Contains(out, "plain") {
				t.Errorf("log.Print output missing; output:\n%s", out)
			}
		})
	}
}

// builtinDefault is slog's builtin default logger, captured before any test
// replaces it. Wrapping its handler deadlocks if the log package's output is
// left redirected into slog.
var builtinDefault = slog.Default()

func restoreDefaultLogger(t *testing.T) {
	t.Helper()
	logger, writer, flags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(logger)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
}
