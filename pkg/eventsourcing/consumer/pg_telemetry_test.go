package consumer_test

import (
	"context"
	"errors"
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
	"github.com/deepworx/go-utils/pkg/postgres/lease"
)

// Two Runtimes play two pods running the same Consumer. Only the lease
// owner reports the Consumer gauges; each pod reports xmin age once.
func TestPgGaugesOnlyFromLeaseOwner(t *testing.T) {
	e := newPgEnv(t, pgEnvConfig{})
	e.write(t, noteWritten{Text: "a"})
	failing := consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error {
		return consumer.Permanent(errors.New("boom"))
	})

	var (
		readers   []*sdkmetric.ManualReader
		consumers []*consumer.Consumer
	)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	for range 2 {
		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		claims := lease.NewClaims(e.pool, lease.Config{PingInterval: 50 * time.Millisecond})
		wg.Go(func() { _ = claims.Run(ctx) })
		rt, err := consumer.NewPgRuntime(consumer.PgRuntimeConfig{
			Pool: e.pool, Store: e.store, Claims: claims, MeterProvider: mp,
		})
		if err != nil {
			t.Fatalf("NewPgRuntime: %v", err)
		}
		c := newPgConsumer(t, rt, consumer.Config{Name: "owner", Handler: failing},
			consumer.WithMeterProvider(mp), consumer.WithPollInterval(20*time.Millisecond))
		readers = append(readers, reader)
		consumers = append(consumers, c)
		wg.Go(func() { _ = c.Run(ctx) })
	}
	waitStatus(t, consumers[0], func(s consumer.Status) bool { return s.Stopped != nil })

	want := attribute.NewSet(attribute.String("eventsourcing.consumer", "owner"))
	owners := 0
	for i, reader := range readers {
		metrics := collect(t, reader)
		if g, ok := metrics["eventsourcing.xmin.age"].(metricdata.Gauge[int64]); !ok || len(g.DataPoints) != 1 {
			t.Errorf("pod %d: xmin age = %#v, want one data point", i, metrics["eventsourcing.xmin.age"])
		} else if g.DataPoints[0].Value < 0 {
			t.Errorf("pod %d: xmin age = %d, want >= 0", i, g.DataPoints[0].Value)
		}
		if len(gaugeNames(metrics)) == 0 {
			continue
		}
		owners++
		if got := int64Gauge(t, metrics, "eventsourcing.consumer.pending", want); got != 1 {
			t.Errorf("pending = %d, want 1", got)
		}
		if got := int64Gauge(t, metrics, "eventsourcing.consumer.stopped", want); got != 1 {
			t.Errorf("stopped = %d, want 1", got)
		}
		if lag := float64Gauge(t, metrics, "eventsourcing.consumer.lag", want); lag <= 0 {
			t.Errorf("lag = %v, want > 0", lag)
		}
	}
	if owners != 1 {
		t.Errorf("%d pods report the Consumer gauges, want exactly 1", owners)
	}
}

func TestPgBatchSpanLinksProducers(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	e := newPgEnv(t, pgEnvConfig{})
	e.notes = eventstore.NewRepository[noteID, note](
		eventstore.RepositoryConfig{Store: e.store, UnitOfWork: e.uow}, eventstore.WithTracerProvider(tp))
	e.write(t, noteWritten{Text: "a"})
	e.write(t, noteWritten{Text: "b"}, noteDeleted{})
	producers := spanIDsOfKind(rec.Ended(), trace.SpanKindProducer)

	for _, filter := range [][]string{nil, {"Test.Note.Written", "Test.Note.Deleted"}} {
		name := "unfiltered"
		if filter != nil {
			name = "filtered"
		}
		c := newPgConsumer(t, e.runtime(t), consumer.Config{Name: name, EventTypes: filter},
			consumer.WithTracerProvider(tp))
		if err := c.Drain(context.Background()); err != nil {
			t.Fatalf("Drain %s: %v", name, err)
		}
		batch := endedSpan(t, rec, "process "+name)
		if batch.Parent().IsValid() {
			t.Errorf("%s: batch span parent = %v, want a new root", name, batch.Parent())
		}
		if got := len(batch.Links()); got != 3 {
			t.Fatalf("%s: links = %d, want 3", name, got)
		}
		for _, l := range batch.Links() {
			if !producers[l.SpanContext.SpanID()] {
				t.Errorf("%s: link %v does not point to a producer span", name, l.SpanContext.SpanID())
			}
		}
	}
}

// Runtimes sharing a MeterProvider report xmin age as one series, and a
// closed Runtime stops reporting it.
func TestPgXminAgeOncePerMeterProvider(t *testing.T) {
	e := newPgEnv(t, pgEnvConfig{})
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	var runtimes []*consumer.PgRuntime
	for range 2 {
		rt, err := consumer.NewPgRuntime(consumer.PgRuntimeConfig{
			Pool: e.pool, Store: e.store, Claims: lease.NewClaims(e.pool, lease.Config{}), MeterProvider: mp,
		})
		if err != nil {
			t.Fatalf("NewPgRuntime: %v", err)
		}
		runtimes = append(runtimes, rt)
	}
	points := func() int {
		g, _ := collect(t, reader)["eventsourcing.xmin.age"].(metricdata.Gauge[int64])
		return len(g.DataPoints)
	}

	steps := []struct {
		name  string
		close int
		want  int
	}{
		{"both open", -1, 1},
		{"one closed", 0, 1},
		{"both closed", 1, 0},
	}
	for _, s := range steps {
		if s.close >= 0 {
			runtimes[s.close].Close()
		}
		if got := points(); got != s.want {
			t.Errorf("%s: xmin age points = %d, want %d", s.name, got, s.want)
		}
	}
}
