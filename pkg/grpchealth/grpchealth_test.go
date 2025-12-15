package grpchealth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

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
	t.Parallel()

	cfg := DefaultConfig()
	agg := NewAggregator(cfg)

	if agg == nil {
		t.Fatal("NewAggregator returned nil")
	}
	if agg.IsServing() {
		t.Error("new aggregator should not be serving initially")
	}
}

func TestRegister(t *testing.T) {
	t.Parallel()

	t.Run("successful registration", func(t *testing.T) {
		t.Parallel()

		agg := NewAggregator(DefaultConfig())
		checker := HealthCheckerFunc(func(ctx context.Context) bool { return true })

		result := agg.Register("test", checker)
		if result != agg {
			t.Error("Register should return aggregator for chaining")
		}
	})

	t.Run("chained registration", func(t *testing.T) {
		t.Parallel()

		agg := NewAggregator(DefaultConfig())
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
		t.Parallel()

		defer func() {
			if r := recover(); r == nil {
				t.Error("Register with empty name should panic")
			}
		}()

		agg := NewAggregator(DefaultConfig())
		checker := HealthCheckerFunc(func(ctx context.Context) bool { return true })
		agg.Register("", checker)
	})

	t.Run("duplicate name panics", func(t *testing.T) {
		t.Parallel()

		defer func() {
			if r := recover(); r == nil {
				t.Error("Register with duplicate name should panic")
			}
		}()

		agg := NewAggregator(DefaultConfig())
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
	t.Parallel()

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
			t.Parallel()

			cfg := Config{
				Interval: 100 * time.Millisecond,
				Timeout:  50 * time.Millisecond,
			}
			agg := NewAggregator(cfg)

			for name, healthy := range tt.checkers {
				h := healthy // capture
				agg.Register(name, HealthCheckerFunc(func(ctx context.Context) bool {
					return h
				}))
			}

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()

			// Run checks once
			agg.runChecks(ctx)

			if agg.IsServing() != tt.expectedServing {
				t.Errorf("IsServing() = %v, want %v", agg.IsServing(), tt.expectedServing)
			}
		})
	}
}

func TestRunChecksTimeout(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Interval: 100 * time.Millisecond,
		Timeout:  50 * time.Millisecond,
	}
	agg := NewAggregator(cfg)

	// Register a checker that blocks longer than timeout
	agg.Register("slow", HealthCheckerFunc(func(ctx context.Context) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
			return true
		}
	}))

	ctx := context.Background()
	agg.runChecks(ctx)

	if agg.IsServing() {
		t.Error("slow checker should result in not serving")
	}
}

func TestRunChecksPanic(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Interval: 100 * time.Millisecond,
		Timeout:  50 * time.Millisecond,
	}
	agg := NewAggregator(cfg)

	agg.Register("panicky", HealthCheckerFunc(func(ctx context.Context) bool {
		panic("test panic")
	}))

	ctx := context.Background()

	// Should not panic
	agg.runChecks(ctx)

	if agg.IsServing() {
		t.Error("panicking checker should result in not serving")
	}
}

func TestRunContextCancellation(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Interval: 50 * time.Millisecond,
		Timeout:  25 * time.Millisecond,
	}
	agg := NewAggregator(cfg)

	agg.Register("test", HealthCheckerFunc(func(ctx context.Context) bool {
		return true
	}))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- agg.Run(ctx)
	}()

	// Let it run a few cycles
	time.Sleep(150 * time.Millisecond)

	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Error("Run() did not return after context cancellation")
	}
}

func TestRunParallelExecution(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Interval: 100 * time.Millisecond,
		Timeout:  500 * time.Millisecond,
	}
	agg := NewAggregator(cfg)

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

	ctx := context.Background()
	agg.runChecks(ctx)

	if maxConcurrent.Load() < 2 {
		t.Errorf("expected parallel execution, max concurrent = %d", maxConcurrent.Load())
	}
}

func TestHandler(t *testing.T) {
	t.Parallel()

	agg := NewAggregator(DefaultConfig())

	path, handler := agg.Handler()

	if path == "" {
		t.Error("Handler() returned empty path")
	}
	if handler == nil {
		t.Error("Handler() returned nil handler")
	}
}
