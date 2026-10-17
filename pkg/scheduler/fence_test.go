package scheduler_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/internal/telemetry"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/scheduler"
)

func TestConcurrentTicksProduceOneRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []scheduler.Option
	}{
		{name: "leases held"},
		{name: "only the claim fence", opts: []scheduler.Option{scheduler.WithoutLeases()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pool := pgtest.New(t)
			var runs atomic.Int32
			job := scheduler.Job{
				Name: "eod",
				Run: func(context.Context, scheduler.Run) (scheduler.Outcome, error) {
					runs.Add(1)
					time.Sleep(50 * time.Millisecond)
					return scheduler.Outcome{}, nil
				},
			}
			schedulers := make([]*scheduler.Scheduler, 4)
			for i := range schedulers {
				schedulers[i] = newRunningSchedulerOn(t, pool, scheduler.Config{Location: berlin(t)}, tt.opts...)
				mustRegister(t, schedulers[i], job)
			}

			now := utc(t, "2026-03-10T08:00:00Z")
			var wg sync.WaitGroup
			for _, s := range schedulers {
				wg.Go(func() {
					if err := s.Tick(t.Context(), now); err != nil {
						t.Errorf("Tick() error = %v", err)
					}
				})
			}
			wg.Wait()

			if got := runs.Load(); got != 1 {
				t.Fatalf("runs = %d, want 1", got)
			}
		})
	}
}

func TestRunContextHasSchedulerIdentity(t *testing.T) {
	t.Parallel()

	spans := tracetest.NewSpanRecorder()
	s := newRunningScheduler(t, scheduler.Config{Location: berlin(t)},
		scheduler.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))))
	var (
		actor, correlation, tenant string
		hasCorrelation, hasTenant  bool
	)
	mustRegister(t, s, scheduler.Job{
		Name: "eod",
		Run: func(ctx context.Context, _ scheduler.Run) (scheduler.Outcome, error) {
			actor, _ = ctxutil.Actor(ctx)
			correlation, hasCorrelation = ctxutil.CorrelationID(ctx)
			tenant, hasTenant = ctxutil.TenantID(ctx)
			return scheduler.Outcome{}, nil
		},
	})

	parent := ctxutil.WithClaims(t.Context(), ctxutil.Claims{TenantID: "claims-tenant", UserID: "u1"})
	parent = ctxutil.WithTenantID(parent, "explicit-tenant")
	parent = ctxutil.WithCorrelationID(parent, "request-correlation")
	if err := s.Tick(parent, utc(t, "2026-03-10T08:00:00Z")); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if actor != "system:scheduler:eod" {
		t.Errorf("Actor = %q, want system:scheduler:eod", actor)
	}
	if !hasCorrelation || correlation == "request-correlation" {
		t.Errorf("CorrelationID = %q, %v, want a fresh one", correlation, hasCorrelation)
	}
	if hasTenant {
		t.Errorf("TenantID = %q, want none", tenant)
	}

	ended := spans.Ended()
	if len(ended) != 1 {
		t.Fatalf("spans = %d, want 1", len(ended))
	}
	span := ended[0]
	if span.Name() != "scheduler.run eod" {
		t.Errorf("span name = %q, want scheduler.run eod", span.Name())
	}
	if span.Parent().IsValid() {
		t.Errorf("span has parent %v, want a root span", span.Parent())
	}
	hasJob := false
	for _, kv := range span.Attributes() {
		if kv.Key == telemetry.AttrJob && kv.Value.AsString() == "eod" {
			hasJob = true
		}
	}
	if !hasJob {
		t.Errorf("span attributes %v lack %s=eod", span.Attributes(), telemetry.AttrJob)
	}
}

func TestRunLoopsUntilContextEnds(t *testing.T) {
	t.Parallel()

	s := newRunningScheduler(t, scheduler.Config{Location: berlin(t), PollInterval: 10 * time.Millisecond})
	ran := make(chan struct{}, 10)
	mustRegister(t, s, scheduler.Job{
		Name: "eod",
		Run: func(context.Context, scheduler.Run) (scheduler.Outcome, error) {
			ran <- struct{}{}
			return scheduler.Outcome{}, nil
		},
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("job did not run")
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if extra := len(ran); extra != 0 {
		t.Fatalf("job ran %d more times on the same Business date", extra)
	}
}
