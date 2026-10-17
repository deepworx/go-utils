package inbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/deepworx/go-utils/pkg/inbox"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
)

func TestShutdownReleasesClaimWithoutCountingAttempt(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	mustInsert(t, store, inbox.Message{Source: "src", Key: "k1", Type: "t"})
	started := make(chan struct{})
	w := newWorker(t, store, inbox.Config{Source: "src", Retries: []time.Duration{}, Handle: func(ctx context.Context, _ inbox.Delivery) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	<-started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := mustGet(t, store, "src", "k1")
	if got.Status != inbox.StatusPending || got.Attempts != 0 || got.Error != "" {
		t.Errorf("message = %+v, want pending with 0 attempts and no error", got)
	}
	if got.NextAttemptAt.After(time.Now()) {
		t.Errorf("NextAttemptAt = %v, want due now", got.NextAttemptAt)
	}
}

func TestExpiredClaimOnLastAttemptParks(t *testing.T) {
	t.Parallel()

	clk := newClock()
	store, _ := newStore(t, inbox.WithClock(clk.Now))
	mustInsert(t, store, inbox.Message{Source: "src", Key: "k1", Type: "t"})

	started, release := make(chan struct{}), make(chan struct{})
	crashed := newWorker(t, store, inbox.Config{
		Source: "src", Timeout: time.Minute, Retries: []time.Duration{},
		Handle: blockingHandler(started, release, nil),
	})
	crashedDone := make(chan error, 1)
	go func() { crashedDone <- crashed.Drain(t.Context()) }()
	<-started

	// The claim of the only attempt expires without a settle, as when the
	// handler took the process down.
	clk.Advance(time.Minute + 30*time.Second)
	rec := &recorder{}
	drain(t, newWorker(t, store, inbox.Config{Source: "src", Retries: []time.Duration{}, Handle: rec.handle(nil)}))

	if got := len(rec.deliveries()); got != 0 {
		t.Errorf("handled %d deliveries after the last attempt, want 0", got)
	}
	got := mustGet(t, store, "src", "k1")
	if got.Status != inbox.StatusParked || got.Attempts != 1 || got.Error == "" {
		t.Errorf("message = %+v, want parked after 1 attempt with an error", got)
	}

	close(release)
	if err := <-crashedDone; err != nil {
		t.Fatalf("crashed Drain() error = %v", err)
	}
	if got := mustGet(t, store, "src", "k1"); got.Status != inbox.StatusParked {
		t.Errorf("after stale settle: status = %q, want parked", got.Status)
	}
}

func TestGaugesStopAfterTelemetryLeaseEnds(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	store, pool := newStore(t)
	mustInsert(t, store, inbox.Message{Source: "src", Key: "k1", Type: "t"})

	claims := lease.NewClaims(pool, lease.Config{})
	startBackground(t, claims.Run)
	waitConnected(t, claims)

	w := newWorker(t, store, inbox.Config{Source: "src", Handle: func(context.Context, inbox.Delivery) error {
		return errors.New("keep pending")
	}}, inbox.WithClaims(claims), inbox.WithMeterProvider(mp), inbox.WithPollInterval(time.Hour))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	deadline := time.Now().Add(waitTimeout)
	for !reportsGauge(t, reader, "inbox.messages") {
		if time.Now().After(deadline) {
			t.Fatal("inbox.messages gauge not reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, name := range []string{"inbox.messages", "inbox.pending.oldest_age"} {
		if reportsGauge(t, reader, name) {
			t.Errorf("%s still reported after the lease ended", name)
		}
	}
}

// reportsGauge reports whether the next collection has data points for
// the gauge name.
func reportsGauge(t *testing.T, reader *sdkmetric.ManualReader, name string) bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	m, ok := findMetric(rm, name)
	if !ok {
		return false
	}
	switch g := m.Data.(type) {
	case metricdata.Gauge[int64]:
		return len(g.DataPoints) > 0
	case metricdata.Gauge[float64]:
		return len(g.DataPoints) > 0
	}
	return false
}
