package telemetry_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/deepworx/go-utils/internal/telemetry"
)

const scope = "github.com/deepworx/go-utils/pkg/eventsourcing/consumer"

func TestSpanNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "create command", got: telemetry.SpanCreate("order"), want: "create order"},
		{name: "update command", got: telemetry.SpanUpdate("order"), want: "update order"},
		{name: "send", got: telemetry.SpanSend("order"), want: "send order"},
		{name: "consumer batch", got: telemetry.SpanProcess("billing"), want: "process billing"},
		{name: "per event", got: telemetry.SpanHandle("OrderPlaced"), want: "handle OrderPlaced"},
		{name: "inbox", got: telemetry.SpanInboxProcess("stripe"), want: "process inbox stripe"},
		{name: "scheduler", got: telemetry.SpanSchedulerRun("close-day"), want: "scheduler.run close-day"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestInstrumentsMatchSpec(t *testing.T) {
	t.Parallel()

	want := []string{
		"eventsourcing.aggregate.load.duration",
		"eventsourcing.aggregate.load.events",
		"eventsourcing.command.duration",
		"eventsourcing.consumer.batch.duration",
		"eventsourcing.consumer.process.duration",
		"inbox.handle.duration",
		"scheduler.run.duration",
		"inbox.messages.handled",
		"scheduler.run.candidate_failures",
		"notify.reconnects",
		"notify.gaps",
		"lease.reconnects",
		"lease.lost",
		"lease.held",
		"eventsourcing.consumer.pending",
		"eventsourcing.consumer.lag",
		"eventsourcing.consumer.stopped",
		"eventsourcing.reactor.parked.streams",
		"eventsourcing.reactor.parked.events",
		"eventsourcing.reactor.parked.exhausted",
		"inbox.messages",
		"inbox.pending.oldest_age",
		"scheduler.job.last_success",
		"eventsourcing.xmin.age",
	}

	var got []string
	for _, inst := range telemetry.Instruments {
		if inst.Unit == "" {
			t.Errorf("%s has no unit", inst.Name)
		}
		got = append(got, inst.Name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("instrument names\n got: %v\nwant: %v", got, want)
	}
}

func TestAttributeKeysAreNamespaced(t *testing.T) {
	t.Parallel()

	seen := map[attribute.Key]bool{}
	for _, key := range telemetry.AttributeKeys {
		if seen[key] {
			t.Errorf("duplicate attribute key %q", key)
		}
		seen[key] = true
		if key != "error.type" && !strings.HasPrefix(string(key), "eventsourcing.") {
			t.Errorf("attribute key %q is outside eventsourcing.*", key)
		}
	}
	if !seen[telemetry.AttrEventTypes] || !seen[telemetry.AttrErrorType] {
		t.Error("AttributeKeys misses eventsourcing.event_types or error.type")
	}
}

func TestLogKeysMatchSpec(t *testing.T) {
	t.Parallel()

	want := []string{
		"tenant_id", "consumer", "stream_type", "stream_id", "event_id",
		"event_type", "position", "attempt", "job", "source", "error",
	}
	if !slices.Equal(telemetry.LogKeys, want) {
		t.Errorf("LogKeys = %v, want %v", telemetry.LogKeys, want)
	}
}

func TestProvidersUseExplicitTracerProvider(t *testing.T) {
	t.Parallel()

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, span := telemetry.Providers{TracerProvider: tp}.Tracer(scope).Start(context.Background(), "process billing")
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	if got := ended[0].InstrumentationScope().Name; got != scope {
		t.Errorf("scope = %q, want %q", got, scope)
	}
}

func TestProvidersWithoutExplicitProvidersAreUsable(t *testing.T) {
	t.Parallel()

	var p telemetry.Providers
	_, span := p.Tracer(scope).Start(context.Background(), "process billing")
	span.End()

	counter, err := telemetry.NotifyReconnects.Int64Counter(p.Meter(scope))
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(context.Background(), 1)
}

func TestInstrumentHelpersApplyRegistry(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	meter := telemetry.Providers{MeterProvider: mp}.Meter(scope)
	ctx := context.Background()

	hist, err := telemetry.ConsumerBatchDuration.Float64Histogram(meter)
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}
	hist.Record(ctx, 0.2)

	events, err := telemetry.AggregateLoadEvents.Int64Histogram(meter)
	if err != nil {
		t.Fatalf("Int64Histogram: %v", err)
	}
	events.Record(ctx, 3)

	held, err := telemetry.LeaseHeld.Int64UpDownCounter(meter)
	if err != nil {
		t.Fatalf("Int64UpDownCounter: %v", err)
	}
	held.Add(ctx, 1)

	pending, err := telemetry.ConsumerPending.Int64Gauge(meter)
	if err != nil {
		t.Fatalf("Int64Gauge: %v", err)
	}
	pending.Record(ctx, 7)

	age, err := telemetry.InboxPendingOldestAge.Float64Gauge(meter)
	if err != nil {
		t.Fatalf("Float64Gauge: %v", err)
	}
	age.Record(ctx, 1.5)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	metrics := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != scope {
			t.Errorf("scope = %q, want %q", sm.Scope.Name, scope)
		}
		for _, m := range sm.Metrics {
			metrics[m.Name] = m
		}
	}

	tests := []struct {
		name       string
		unit       string
		boundaries []float64
	}{
		{name: "eventsourcing.consumer.batch.duration", unit: "s", boundaries: telemetry.DurationBoundaries},
		{name: "eventsourcing.aggregate.load.events", unit: "{event}", boundaries: telemetry.EventCountBoundaries},
		{name: "lease.held", unit: "{lease}"},
		{name: "eventsourcing.consumer.pending", unit: "{event}"},
		{name: "inbox.pending.oldest_age", unit: "s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m, ok := metrics[tt.name]
			if !ok {
				t.Fatalf("metric %q not collected", tt.name)
			}
			if m.Unit != tt.unit {
				t.Errorf("unit = %q, want %q", m.Unit, tt.unit)
			}
			if tt.boundaries == nil {
				return
			}
			if got := histogramBounds(t, m); !slices.Equal(got, tt.boundaries) {
				t.Errorf("boundaries = %v, want %v", got, tt.boundaries)
			}
		})
	}
}

func histogramBounds(t *testing.T, m metricdata.Metrics) []float64 {
	t.Helper()

	switch data := m.Data.(type) {
	case metricdata.Histogram[float64]:
		return data.DataPoints[0].Bounds
	case metricdata.Histogram[int64]:
		return data.DataPoints[0].Bounds
	default:
		t.Fatalf("%s is %T, not a histogram", m.Name, m.Data)
		return nil
	}
}
