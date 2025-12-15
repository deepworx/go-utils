package grpchealth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepworx/go-utils/pkg/shutdown"
)

// cleanupShutdown clears the global shutdown handlers after each test.
func cleanupShutdown(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = shutdown.Shutdown(context.Background())
	})
}

func TestDefaultConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	if cfg.Interval != 10*time.Second {
		t.Errorf("Interval = %v, want %v", cfg.Interval, 10*time.Second)
	}
	if cfg.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want %v", cfg.Timeout, 5*time.Second)
	}
}

func TestNewAggregator(t *testing.T) {
	cleanupShutdown(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := DefaultConfig()
	agg := NewAggregator(ctx, cfg)

	if agg == nil {
		t.Fatal("NewAggregator returned nil")
	}
	if agg.IsServing() {
		t.Error("new aggregator should not be serving initially")
	}
}

func TestNewAggregator_StartsBackgroundLoop(t *testing.T) {
	cleanupShutdown(t)

	cfg := Config{
		Interval: 50 * time.Millisecond,
		Timeout:  25 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var checkCount atomic.Int32
	checker := HealthCheckerFunc(func(ctx context.Context) bool {
		checkCount.Add(1)
		return true
	})

	agg := NewAggregator(ctx, cfg)
	agg.Register("test", checker)

	// Wait for background loop to run a few cycles
	time.Sleep(150 * time.Millisecond)

	if checkCount.Load() < 2 {
		t.Errorf("expected at least 2 health checks, got %d", checkCount.Load())
	}
	if !agg.IsServing() {
		t.Error("aggregator should be serving after successful checks")
	}
}

func TestRegister(t *testing.T) {
	t.Run("successful registration", func(t *testing.T) {
		cleanupShutdown(t)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		agg := NewAggregator(ctx, DefaultConfig())
		checker := HealthCheckerFunc(func(ctx context.Context) bool { return true })

		result := agg.Register("test", checker)
		if result != agg {
			t.Error("Register should return aggregator for chaining")
		}
	})

	t.Run("chained registration", func(t *testing.T) {
		cleanupShutdown(t)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		agg := NewAggregator(ctx, DefaultConfig())
		checker := HealthCheckerFunc(func(ctx context.Context) bool { return true })

		result := agg.
			Register("service1", checker).
			Register("service2", checker).
			Register("service3", checker)

		if result != agg {
			t.Error("chained Register should return aggregator")
		}
	})

	t.Run("empty name panics", func(t *testing.T) {
		cleanupShutdown(t)

		defer func() {
			if r := recover(); r == nil {
				t.Error("Register with empty name should panic")
			}
		}()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		agg := NewAggregator(ctx, DefaultConfig())
		checker := HealthCheckerFunc(func(ctx context.Context) bool { return true })
		agg.Register("", checker)
	})

	t.Run("duplicate name panics", func(t *testing.T) {
		cleanupShutdown(t)

		defer func() {
			if r := recover(); r == nil {
				t.Error("Register with duplicate name should panic")
			}
		}()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		agg := NewAggregator(ctx, DefaultConfig())
		checker := HealthCheckerFunc(func(ctx context.Context) bool { return true })
		agg.Register("test", checker)
		agg.Register("test", checker)
	})
}

func TestHealthCheckerFunc(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fn       HealthCheckerFunc
		expected bool
	}{
		{
			name:     "returns true",
			fn:       func(ctx context.Context) bool { return true },
			expected: true,
		},
		{
			name:     "returns false",
			fn:       func(ctx context.Context) bool { return false },
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := tt.fn.Check(context.Background())
			if result != tt.expected {
				t.Errorf("Check() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestRunChecks(t *testing.T) {
	tests := []struct {
		name            string
		checkers        map[string]bool
		expectedServing bool
	}{
		{
			name:            "no checkers returns serving",
			checkers:        map[string]bool{},
			expectedServing: true,
		},
		{
			name:            "single healthy checker",
			checkers:        map[string]bool{"db": true},
			expectedServing: true,
		},
		{
			name:            "single unhealthy checker",
			checkers:        map[string]bool{"db": false},
			expectedServing: false,
		},
		{
			name:            "all healthy checkers",
			checkers:        map[string]bool{"db": true, "cache": true, "queue": true},
			expectedServing: true,
		},
		{
			name:            "one unhealthy checker",
			checkers:        map[string]bool{"db": true, "cache": false, "queue": true},
			expectedServing: false,
		},
		{
			name:            "all unhealthy checkers",
			checkers:        map[string]bool{"db": false, "cache": false},
			expectedServing: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanupShutdown(t)

			cfg := Config{
				Interval: 100 * time.Millisecond,
				Timeout:  50 * time.Millisecond,
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			agg := NewAggregator(ctx, cfg)

			for name, healthy := range tt.checkers {
				h := healthy // capture
				agg.Register(name, HealthCheckerFunc(func(ctx context.Context) bool {
					return h
				}))
			}

			checkCtx, checkCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer checkCancel()

			// Run checks once
			agg.runChecks(checkCtx)

			if agg.IsServing() != tt.expectedServing {
				t.Errorf("IsServing() = %v, want %v", agg.IsServing(), tt.expectedServing)
			}
		})
	}
}

func TestRunChecksTimeout(t *testing.T) {
	cleanupShutdown(t)

	cfg := Config{
		Interval: 100 * time.Millisecond,
		Timeout:  50 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agg := NewAggregator(ctx, cfg)

	// Register a checker that blocks longer than timeout
	agg.Register("slow", HealthCheckerFunc(func(ctx context.Context) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
			return true
		}
	}))

	agg.runChecks(context.Background())

	if agg.IsServing() {
		t.Error("slow checker should result in not serving")
	}
}

func TestRunChecksPanic(t *testing.T) {
	cleanupShutdown(t)

	cfg := Config{
		Interval: 100 * time.Millisecond,
		Timeout:  50 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agg := NewAggregator(ctx, cfg)

	agg.Register("panicky", HealthCheckerFunc(func(ctx context.Context) bool {
		panic("test panic")
	}))

	// Should not panic
	agg.runChecks(context.Background())

	if agg.IsServing() {
		t.Error("panicking checker should result in not serving")
	}
}

func TestContextCancellationStopsAggregator(t *testing.T) {
	cleanupShutdown(t)

	cfg := Config{
		Interval: 50 * time.Millisecond,
		Timeout:  25 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())

	var checkCount atomic.Int32
	checker := HealthCheckerFunc(func(ctx context.Context) bool {
		checkCount.Add(1)
		return true
	})

	agg := NewAggregator(ctx, cfg)
	agg.Register("test", checker)

	// Let it run a few cycles
	time.Sleep(150 * time.Millisecond)
	countBeforeCancel := checkCount.Load()

	cancel()

	// Wait a bit and verify no more checks happen
	time.Sleep(150 * time.Millisecond)
	countAfterCancel := checkCount.Load()

	if countAfterCancel > countBeforeCancel+1 {
		t.Errorf("expected checks to stop after cancel, before=%d after=%d", countBeforeCancel, countAfterCancel)
	}
}

func TestShutdownStopsAggregator(t *testing.T) {
	// Note: not using cleanupShutdown here as we test shutdown explicitly

	cfg := Config{
		Interval: 50 * time.Millisecond,
		Timeout:  25 * time.Millisecond,
	}

	ctx := context.Background()

	var checkCount atomic.Int32
	checker := HealthCheckerFunc(func(ctx context.Context) bool {
		checkCount.Add(1)
		return true
	})

	agg := NewAggregator(ctx, cfg)
	agg.Register("test", checker)

	// Let it run a few cycles
	time.Sleep(150 * time.Millisecond)
	countBeforeShutdown := checkCount.Load()

	// Trigger shutdown
	_ = shutdown.Shutdown(context.Background())

	// Wait a bit and verify no more checks happen
	time.Sleep(150 * time.Millisecond)
	countAfterShutdown := checkCount.Load()

	if countAfterShutdown > countBeforeShutdown+1 {
		t.Errorf("expected checks to stop after shutdown, before=%d after=%d", countBeforeShutdown, countAfterShutdown)
	}
}

func TestRunParallelExecution(t *testing.T) {
	cleanupShutdown(t)

	cfg := Config{
		Interval: 100 * time.Millisecond,
		Timeout:  500 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agg := NewAggregator(ctx, cfg)

	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32

	for i := 0; i < 5; i++ {
		agg.Register(string(rune('a'+i)), HealthCheckerFunc(func(ctx context.Context) bool {
			c := concurrent.Add(1)
			defer concurrent.Add(-1)

			// Track max concurrent
			for {
				old := maxConcurrent.Load()
				if c <= old || maxConcurrent.CompareAndSwap(old, c) {
					break
				}
			}

			time.Sleep(50 * time.Millisecond)
			return true
		}))
	}

	agg.runChecks(context.Background())

	if maxConcurrent.Load() < 2 {
		t.Errorf("expected parallel execution, max concurrent = %d", maxConcurrent.Load())
	}
}

func TestHandler(t *testing.T) {
	cleanupShutdown(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agg := NewAggregator(ctx, DefaultConfig())

	path, handler := agg.Handler()

	if path == "" {
		t.Error("Handler() returned empty path")
	}
	if handler == nil {
		t.Error("Handler() returned nil handler")
	}
}
