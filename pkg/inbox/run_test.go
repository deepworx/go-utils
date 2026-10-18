package inbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/pkg/inbox"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

const waitTimeout = 10 * time.Second

// startRun runs w until the test ends and fails the test if Run errors.
func startRun(t *testing.T, w *inbox.Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() error = %v", err)
		}
	})
}

func startBackground(t *testing.T, run func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() error = %v", err)
		}
	})
}

func TestDrainReturnsErrRunningWhileRunIsActive(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	mustInsert(t, store, inbox.Message{Source: "src", Key: "k1", Type: "t"})
	started, release := make(chan struct{}), make(chan struct{})
	w := newWorker(t, store, inbox.Config{Source: "src", Handle: blockingHandler(started, release, nil)})
	startRun(t, w)
	t.Cleanup(func() { close(release) })
	<-started

	if err := w.Drain(t.Context()); !errors.Is(err, inbox.ErrRunning) {
		t.Errorf("Drain() error = %v, want ErrRunning", err)
	}
	if err := w.Run(t.Context()); !errors.Is(err, inbox.ErrRunning) {
		t.Errorf("second Run() error = %v, want ErrRunning", err)
	}
}

func TestRunWakesUpOnInsertNotification(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	listener := notify.NewListener(pool, notify.Config{})
	startBackground(t, listener.Run)

	handled := make(chan inbox.Delivery, 1)
	w := newWorker(t, store, inbox.Config{Source: "src", Handle: func(_ context.Context, d inbox.Delivery) error {
		handled <- d
		return nil
	}}, inbox.WithListener(listener), inbox.WithPollInterval(time.Hour))
	startRun(t, w)

	// LISTEN is asynchronous: keep inserting until one notification arrives.
	deadline := time.After(waitTimeout)
	for i := 0; ; i++ {
		mustInsert(t, store, inbox.Message{Source: "src", Key: time.Now().String(), Type: "t"})
		select {
		case <-handled:
			return
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatalf("no message handled after %d inserts; Run did not wake up", i+1)
		}
	}
}

func TestDeliverySpanLinksToInsertingSpan(t *testing.T) {
	t.Parallel()

	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	store, _ := newStore(t)
	insertCtx, insertSpan := tp.Tracer("test").Start(t.Context(), "webhook")
	mustInsertCtx(t, insertCtx, store, inbox.Message{Source: "src", Key: "k1", Type: "t", Tenant: "acme"})
	insertSpan.End()

	w := newWorker(t, store, inbox.Config{Source: "src", Handle: func(context.Context, inbox.Delivery) error { return nil }},
		inbox.WithTracerProvider(tp), inbox.WithMeterProvider(mp))
	drain(t, w)

	var process sdktrace.ReadOnlySpan
	for _, s := range spans.Ended() {
		if s.Name() == "process inbox src" {
			process = s
		}
	}
	if process == nil {
		t.Fatal("no span named \"process inbox src\"")
	}
	if process.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("span kind = %v, want consumer", process.SpanKind())
	}
	if process.Parent().IsValid() {
		t.Error("process span has a parent, want a new root")
	}
	links := process.Links()
	if len(links) != 1 || links[0].SpanContext.SpanID() != insertSpan.SpanContext().SpanID() {
		t.Errorf("links = %+v, want one link to the inserting span", links)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if got := counterValue(rm, "inbox.messages.handled", attribute.String("eventsourcing.outcome", "done")); got != 1 {
		t.Errorf("inbox.messages.handled{outcome=done} = %d, want 1", got)
	}
	if !hasMetric(rm, "inbox.handle.duration") {
		t.Error("inbox.handle.duration not recorded")
	}
}

func TestRunReportsGaugesWhileHoldingTelemetryLease(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	store, pool := newStore(t)
	mustInsert(t, store, inbox.Message{Source: "src", Key: "permanent", Type: "t"})
	mustInsert(t, store, inbox.Message{Source: "src", Key: "no-retries-left", Type: "t"})
	setup := newWorker(t, store, inbox.Config{Source: "src", Retries: []time.Duration{}, Handle: func(_ context.Context, d inbox.Delivery) error {
		if d.Key == "permanent" {
			return inbox.Permanent(errors.New("bad"))
		}
		return errors.New("fail")
	}})
	drain(t, setup)
	mustInsert(t, store, inbox.Message{Source: "src", Key: "in-flight", Type: "t"})

	claims := lease.NewClaims(pool, lease.Config{})
	startBackground(t, claims.Run)
	waitConnected(t, claims)

	w := newWorker(t, store, inbox.Config{Source: "src", Handle: func(ctx context.Context, _ inbox.Delivery) error {
		<-ctx.Done()
		return ctx.Err()
	}}, inbox.WithClaims(claims), inbox.WithMeterProvider(mp), inbox.WithPollInterval(time.Hour))
	startRun(t, w)

	deadline := time.Now().Add(waitTimeout)
	for {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(t.Context(), &rm); err != nil {
			t.Fatalf("Collect() error = %v", err)
		}
		if hasMetric(rm, "inbox.messages") {
			if got := gaugeValue(rm, "inbox.messages", attribute.String("eventsourcing.status", "parked")); got != 2 {
				t.Errorf("inbox.messages{status=parked} = %v, want 2", got)
			}
			if got := gaugeValue(rm, "inbox.messages", attribute.String("eventsourcing.status", "pending")); got != 1 {
				t.Errorf("inbox.messages{status=pending} = %v, want 1", got)
			}
			if got := gaugeValue(rm, "inbox.messages", attribute.String("eventsourcing.status", "done")); got != 0 {
				t.Errorf("inbox.messages{status=done} = %v, want 0", got)
			}
			if !hasMetric(rm, "inbox.pending.oldest_age") {
				t.Error("inbox.pending.oldest_age not reported")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("inbox.messages gauge not reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitConnected(t *testing.T, claims *lease.Claims) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		_, l, err := claims.TryAcquire(t.Context(), lease.Key{Namespace: "test", Name: "probe"})
		if err == nil {
			l.Release()
			return
		}
		if !errors.Is(err, lease.ErrNotConnected) || time.Now().After(deadline) {
			t.Fatalf("TryAcquire(probe) error = %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mustInsertCtx(t *testing.T, ctx context.Context, store *inbox.Store, m inbox.Message) {
	t.Helper()
	inserted, err := store.Insert(ctx, m)
	if err != nil || !inserted {
		t.Fatalf("Insert(%s/%s) = %v, %v; want true, nil", m.Source, m.Key, inserted, err)
	}
}

func findMetric(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func hasMetric(rm metricdata.ResourceMetrics, name string) bool {
	_, ok := findMetric(rm, name)
	return ok
}

func counterValue(rm metricdata.ResourceMetrics, name string, attr attribute.KeyValue) int64 {
	m, _ := findMetric(rm, name)
	sum, _ := m.Data.(metricdata.Sum[int64])
	var total int64
	for _, dp := range sum.DataPoints {
		if v, ok := dp.Attributes.Value(attr.Key); ok && v == attr.Value {
			total += dp.Value
		}
	}
	return total
}

func gaugeValue(rm metricdata.ResourceMetrics, name string, attr attribute.KeyValue) int64 {
	m, _ := findMetric(rm, name)
	g, _ := m.Data.(metricdata.Gauge[int64])
	for _, dp := range g.DataPoints {
		if v, ok := dp.Attributes.Value(attr.Key); ok && v == attr.Value {
			return dp.Value
		}
	}
	return -1
}
