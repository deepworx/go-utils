package consumer_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// tracedEnv is an in-memory env whose Repository records its spans on rec.
func tracedEnv(rec *tracetest.SpanRecorder) (env, trace.TracerProvider) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	e := newEnv()
	store := eventstore.NewInMemoryStore(e.codec)
	e.notes = eventstore.NewRepository[noteID, note](
		eventstore.RepositoryConfig{Store: store, UnitOfWork: e.uow}, eventstore.WithTracerProvider(tp))
	e.rt = consumer.NewInMemoryRuntime(consumer.InMemoryRuntimeConfig{Store: store})
	return e, tp
}

func TestBatchSpanLinksProducers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		events    int
		wantLinks int
	}{
		{"FewEvents", 3, 3},
		{"CappedAt128", 130, 128},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := tracetest.NewSpanRecorder()
			e, tp := tracedEnv(rec)
			for range tt.events {
				e.write(t, writerCtx(), "a")
			}
			producers := spanIDsOfKind(rec.Ended(), trace.SpanKindProducer)
			if len(producers) != tt.events {
				t.Fatalf("producer spans = %d, want %d", len(producers), tt.events)
			}

			c, err := consumer.New(e.rt,
				consumer.Config{Name: "links", Mode: consumer.Transactional, Handler: consumer.HandlerFunc(noop)},
				quiet, consumer.WithTracerProvider(tp), consumer.WithBatchSize(200))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := c.Drain(writerCtx()); err != nil {
				t.Fatalf("Drain: %v", err)
			}

			batch := endedSpan(t, rec, "process links")
			if batch.Parent().IsValid() {
				t.Errorf("batch span parent = %v, want a new root", batch.Parent())
			}
			if len(batch.Links()) != tt.wantLinks {
				t.Fatalf("links = %d, want %d", len(batch.Links()), tt.wantLinks)
			}
			for _, l := range batch.Links() {
				if !producers[l.SpanContext.SpanID()] {
					t.Errorf("link %v does not point to a producer span", l.SpanContext.SpanID())
				}
			}
		})
	}
}

func spanIDsOfKind(spans []sdktrace.ReadOnlySpan, kind trace.SpanKind) map[trace.SpanID]bool {
	ids := make(map[trace.SpanID]bool)
	for _, s := range spans {
		if s.SpanKind() == kind {
			ids[s.SpanContext().SpanID()] = true
		}
	}
	return ids
}

func endedSpan(t *testing.T, rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no ended span %q", name)
	return nil
}

func TestHistograms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mode    consumer.Mode
		batches uint64
	}{
		{"Transactional", consumer.Transactional, 2},
		{"OwnTransaction", consumer.OwnTransaction, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv()
			for range 3 {
				e.write(t, writerCtx(), "a")
			}
			reader := sdkmetric.NewManualReader()
			c, err := consumer.New(e.rt,
				consumer.Config{Name: "hist", Mode: tt.mode, Handler: consumer.HandlerFunc(noop)},
				quiet, consumer.WithBatchSize(2),
				consumer.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := c.Drain(context.Background()); err != nil {
				t.Fatalf("Drain: %v", err)
			}

			metrics := collect(t, reader)
			want := attribute.NewSet(attribute.String("eventsourcing.consumer", "hist"))
			for name, count := range map[string]uint64{
				"eventsourcing.consumer.batch.duration":   tt.batches,
				"eventsourcing.consumer.process.duration": 3,
			} {
				points := histogramPoints(t, metrics, name)
				if len(points) != 1 {
					t.Fatalf("%s: %d series, want 1", name, len(points))
				}
				if !points[0].Attributes.Equals(&want) {
					t.Errorf("%s labels = %v, want only the consumer", name, points[0].Attributes.ToSlice())
				}
				if points[0].Count != count {
					t.Errorf("%s count = %d, want %d", name, points[0].Count, count)
				}
			}
		})
	}
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Aggregation {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := make(map[string]metricdata.Aggregation)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m.Data
		}
	}
	return out
}

func histogramPoints(t *testing.T, metrics map[string]metricdata.Aggregation, name string) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	h, ok := metrics[name].(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("%s: got %T, want a float64 histogram", name, metrics[name])
	}
	return h.DataPoints
}

func TestGaugesWhileRunning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		handler     consumer.HandlerFunc
		wantPending int64
		wantStopped int64
		wantLag     bool
	}{
		{"CaughtUp", noop, 0, 0, false},
		{"StoppedAfterFailure", func(context.Context, eventstore.Envelope) error {
			return consumer.Permanent(errors.New("boom"))
		}, 2, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv()
			e.write(t, writerCtx(), "a")
			e.write(t, writerCtx(), "b")
			reader := sdkmetric.NewManualReader()
			c, err := consumer.New(e.rt,
				consumer.Config{Name: "gauges", Mode: consumer.Transactional, Handler: tt.handler},
				quiet, consumer.WithPollInterval(10*time.Millisecond),
				consumer.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := gaugeNames(collect(t, reader)); len(got) != 0 {
				t.Fatalf("gauges before Run = %v, want none", got)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- c.Run(ctx) }()
			waitStatus(t, c, func(s consumer.Status) bool {
				return s.Stopped != nil || s.Pending == 0
			})

			metrics := collect(t, reader)
			want := attribute.NewSet(attribute.String("eventsourcing.consumer", "gauges"))
			if got := int64Gauge(t, metrics, "eventsourcing.consumer.pending", want); got != tt.wantPending {
				t.Errorf("pending = %d, want %d", got, tt.wantPending)
			}
			if got := int64Gauge(t, metrics, "eventsourcing.consumer.stopped", want); got != tt.wantStopped {
				t.Errorf("stopped = %d, want %d", got, tt.wantStopped)
			}
			if lag := float64Gauge(t, metrics, "eventsourcing.consumer.lag", want); (lag > 0) != tt.wantLag {
				t.Errorf("lag = %v, want positive: %v", lag, tt.wantLag)
			}

			cancel()
			if err := <-done; err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := gaugeNames(collect(t, reader)); len(got) != 0 {
				t.Errorf("gauges after Run = %v, want none", got)
			}
		})
	}
}

func TestFailureLogsCarryEventIdentity(t *testing.T) {
	t.Parallel()
	e := newEnv()
	pos := e.write(t, writerCtx(), "a").Position
	logs := newCaptureHandler()
	failing := consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error { return errors.New("boom") })
	c, err := consumer.New(e.rt, consumer.Config{Name: "logs", Mode: consumer.OwnTransaction, Handler: failing},
		consumer.WithLogger(slog.New(logs)),
		consumer.WithRetry(consumer.Retry{MaxAttempts: 2, Base: time.Millisecond, Max: time.Millisecond}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Drain(context.Background()); !errors.Is(err, consumer.ErrStopped) {
		t.Fatalf("Drain = %v, want ErrStopped", err)
	}

	records := logs.all()
	if len(records) != 2 {
		t.Fatalf("log records = %d, want a retry and a stop", len(records))
	}
	for _, r := range records {
		attrs := map[string]string{}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.String()
			return true
		})
		want := map[string]string{
			"consumer":   "logs",
			"event_type": "Test.Note.Written",
			"position":   strconv.FormatInt(pos.Position, 10),
		}
		for k, v := range want {
			if attrs[k] != v {
				t.Errorf("%q: %s = %q, want %q", r.Message, k, attrs[k], v)
			}
		}
		if attrs["event_id"] == "" {
			t.Errorf("%q: no event_id", r.Message)
		}
	}
}

// captureHandler records log records, with the attributes bound by With,
// into a log shared by all derived handlers.
type captureHandler struct {
	log   *recordLog
	bound []slog.Attr
}

type recordLog struct {
	mu      sync.Mutex
	records []slog.Record
}

func newCaptureHandler() *captureHandler { return &captureHandler{log: &recordLog{}} }

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(h.bound...)
	h.log.mu.Lock()
	defer h.log.mu.Unlock()
	h.log.records = append(h.log.records, r)
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureHandler{log: h.log, bound: append(slices.Clone(h.bound), attrs...)}
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

func (h *captureHandler) all() []slog.Record {
	h.log.mu.Lock()
	defer h.log.mu.Unlock()
	return slices.Clone(h.log.records)
}

func waitStatus(t *testing.T, c *consumer.Consumer, done func(consumer.Status) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, err := c.Status(context.Background())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if done(s) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %+v did not reach the wanted state", s)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var gaugeMetrics = []string{
	"eventsourcing.consumer.pending", "eventsourcing.consumer.lag", "eventsourcing.consumer.stopped",
}

// gaugeNames returns the Consumer gauges in metrics that have data points.
func gaugeNames(metrics map[string]metricdata.Aggregation) []string {
	var names []string
	for _, name := range gaugeMetrics {
		switch g := metrics[name].(type) {
		case metricdata.Gauge[int64]:
			if len(g.DataPoints) > 0 {
				names = append(names, name)
			}
		case metricdata.Gauge[float64]:
			if len(g.DataPoints) > 0 {
				names = append(names, name)
			}
		}
	}
	return names
}

func int64Gauge(t *testing.T, metrics map[string]metricdata.Aggregation, name string, attrs attribute.Set) int64 {
	t.Helper()
	g, ok := metrics[name].(metricdata.Gauge[int64])
	if !ok || len(g.DataPoints) != 1 {
		t.Fatalf("%s = %#v, want one int64 data point", name, metrics[name])
	}
	if !g.DataPoints[0].Attributes.Equals(&attrs) {
		t.Errorf("%s labels = %v, want %v", name, g.DataPoints[0].Attributes.ToSlice(), attrs.ToSlice())
	}
	return g.DataPoints[0].Value
}

func float64Gauge(t *testing.T, metrics map[string]metricdata.Aggregation, name string, attrs attribute.Set) float64 {
	t.Helper()
	g, ok := metrics[name].(metricdata.Gauge[float64])
	if !ok || len(g.DataPoints) != 1 {
		t.Fatalf("%s = %#v, want one float64 data point", name, metrics[name])
	}
	if !g.DataPoints[0].Attributes.Equals(&attrs) {
		t.Errorf("%s labels = %v, want %v", name, g.DataPoints[0].Attributes.ToSlice(), attrs.ToSlice())
	}
	return g.DataPoints[0].Value
}
