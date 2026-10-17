package lease_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

func TestTryAcquireRejectsInvalidUse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		run     bool
		key     lease.Key
		wantErr error
	}{
		{name: "before Run has connected", key: lease.Key{Namespace: "ns", Name: "job"}, wantErr: lease.ErrNotConnected},
		{name: "empty name", run: true, key: lease.Key{Namespace: "ns"}, wantErr: lease.ErrEmptyName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			claims := lease.NewClaims(pgtest.New(t), lease.Config{})
			if tt.run {
				startClaims(t, claims)
			}

			_, _, err := claims.TryAcquire(t.Context(), tt.key)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("TryAcquire() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestTryAcquireExcludesOtherHolders(t *testing.T) {
	t.Parallel()

	key := lease.Key{Namespace: "ns", Name: "job"}
	tests := []struct {
		name        string
		sameProcess bool
	}{
		{name: "within one process", sameProcess: true},
		{name: "across two Claims"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pool := pgtest.New(t)
			first := lease.NewClaims(pool, lease.Config{})
			startClaims(t, first)
			second := first
			if !tt.sameProcess {
				second = lease.NewClaims(pool, lease.Config{})
				startClaims(t, second)
			}

			ctx, held := mustAcquire(t, first, key)

			if _, _, err := second.TryAcquire(t.Context(), key); !errors.Is(err, lease.ErrHeld) {
				t.Fatalf("second TryAcquire() error = %v, want ErrHeld", err)
			}

			held.Release()
			if cause := context.Cause(ctx); !errors.Is(cause, context.Canceled) {
				t.Fatalf("lease ctx cause after Release = %v, want context.Canceled", cause)
			}
			mustAcquire(t, second, key)
		})
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	key := lease.Key{Namespace: "ns", Name: "job"}
	pool := pgtest.New(t)
	claims := lease.NewClaims(pool, lease.Config{})
	startClaims(t, claims)
	other := lease.NewClaims(pool, lease.Config{})
	startClaims(t, other)

	_, first := mustAcquire(t, claims, key)
	first.Release()
	mustAcquire(t, claims, key)

	first.Release()

	if _, _, err := other.TryAcquire(t.Context(), key); !errors.Is(err, lease.ErrHeld) {
		t.Fatalf("other TryAcquire() after stale Release error = %v, want ErrHeld", err)
	}
}

func TestBackendKillLosesEveryLeaseAndReconnects(t *testing.T) {
	t.Parallel()

	pool := pgtest.New(t)
	reader := sdkmetric.NewManualReader()
	claims := lease.NewClaims(pool, lease.Config{PingInterval: 20 * time.Millisecond},
		lease.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	startClaims(t, claims)

	keys := []lease.Key{{Namespace: "ns", Name: "a"}, {Namespace: "ns", Name: "b"}}
	var leases []*lease.Lease
	var ctxs []context.Context
	for _, key := range keys {
		ctx, l := mustAcquire(t, claims, key)
		ctxs = append(ctxs, ctx)
		leases = append(leases, l)
	}

	terminateLockBackend(t, pool)

	for i, ctx := range ctxs {
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
			t.Fatalf("lease %v ctx not cancelled after backend kill", keys[i])
		}
		if cause := context.Cause(ctx); !errors.Is(cause, lease.ErrLost) {
			t.Fatalf("lease %v ctx cause = %v, want ErrLost", keys[i], cause)
		}
	}

	for _, l := range leases {
		l.Release()
	}

	waitConnected(t, claims)
	mustAcquire(t, claims, keys[0])

	want := map[string]int64{"lease.lost": 2, "lease.reconnects": 1, "lease.held": 1}
	got := sums(t, reader)
	for name, v := range want {
		if got[name] != v {
			t.Errorf("metric %s = %d, want %d", name, got[name], v)
		}
	}
}

func TestRunShutdownLosesLeasesAndRejectsSecondRun(t *testing.T) {
	t.Parallel()

	claims := lease.NewClaims(pgtest.New(t), lease.Config{})
	runCtx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- claims.Run(runCtx) }()
	waitConnected(t, claims)

	if err := claims.Run(t.Context()); !errors.Is(err, lease.ErrRunning) {
		t.Fatalf("second Run() error = %v, want ErrRunning", err)
	}

	ctx, held := mustAcquire(t, claims, lease.Key{Namespace: "ns", Name: "job"})
	stop()
	if err := <-done; err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	if cause := context.Cause(ctx); !errors.Is(cause, lease.ErrLost) {
		t.Fatalf("lease ctx cause after shutdown = %v, want ErrLost", cause)
	}
	held.Release()
	if _, _, err := claims.TryAcquire(t.Context(), lease.Key{Namespace: "ns", Name: "job"}); !errors.Is(err, lease.ErrNotConnected) {
		t.Fatalf("TryAcquire() after shutdown error = %v, want ErrNotConnected", err)
	}
}

// terminateLockBackend kills the only session holding advisory locks in the
// test database.
func terminateLockBackend(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	var terminated bool
	err := pool.QueryRow(t.Context(), `
		SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND granted
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		GROUP BY pid`).Scan(&terminated)
	if err != nil || !terminated {
		t.Fatalf("terminate lock backend: terminated=%v err=%v", terminated, err)
	}
}

func sums(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	got := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					got[m.Name] += dp.Value
				}
			}
		}
	}
	return got
}

func mustAcquire(t *testing.T, claims *lease.Claims, key lease.Key) (context.Context, *lease.Lease) {
	t.Helper()

	ctx, l, err := claims.TryAcquire(t.Context(), key)
	if err != nil {
		t.Fatalf("TryAcquire(%v) error = %v", key, err)
	}
	t.Cleanup(l.Release)
	return ctx, l
}

// startClaims runs claims until the test ends and waits until it has
// connected.
func startClaims(t *testing.T, claims *lease.Claims) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- claims.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
	})
	waitConnected(t, claims)
}

// waitConnected probes with a key no test uses until TryAcquire stops
// returning ErrNotConnected.
func waitConnected(t *testing.T, claims *lease.Claims) {
	t.Helper()

	probe := lease.Key{Namespace: "lease_test", Name: "probe"}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, l, err := claims.TryAcquire(t.Context(), probe)
		if err == nil {
			l.Release()
			return
		}
		if !errors.Is(err, lease.ErrNotConnected) {
			t.Fatalf("probe TryAcquire() = %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("Claims did not connect within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
