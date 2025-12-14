package shutdown

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestShutdownLIFO(t *testing.T) {
	// Reset global state
	handlers = nil

	var order []int
	Register(func(ctx context.Context) error {
		order = append(order, 1)
		return nil
	})
	Register(func(ctx context.Context) error {
		order = append(order, 2)
		return nil
	})
	Register(func(ctx context.Context) error {
		order = append(order, 3)
		return nil
	})

	err := Shutdown(context.Background())
	if err != nil {
		t.Errorf("Shutdown() error = %v, want nil", err)
	}

	// LIFO: 3, 2, 1
	if len(order) != 3 || order[0] != 3 || order[1] != 2 || order[2] != 1 {
		t.Errorf("Shutdown() order = %v, want [3 2 1]", order)
	}
}

func TestShutdownCollectsErrors(t *testing.T) {
	handlers = nil

	errA := errors.New("error A")
	errB := errors.New("error B")

	Register(func(ctx context.Context) error { return errA })
	Register(func(ctx context.Context) error { return nil })
	Register(func(ctx context.Context) error { return errB })

	err := Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown() error = nil, want combined error")
	}

	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Errorf("Shutdown() error should contain both errA and errB, got: %v", err)
	}
}

func TestShutdownRespectsContext(t *testing.T) {
	handlers = nil

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var ctxReceived context.Context
	Register(func(ctx context.Context) error {
		ctxReceived = ctx
		return nil
	})

	_ = Shutdown(ctx)

	if ctxReceived != ctx {
		t.Error("Handler did not receive the provided context")
	}
}

func TestShutdownClearsHandlers(t *testing.T) {
	handlers = nil

	Register(func(ctx context.Context) error { return nil })
	_ = Shutdown(context.Background())

	if len(handlers) != 0 {
		t.Errorf("Shutdown() should clear handlers, got %d", len(handlers))
	}
}

func TestShutdownEmpty(t *testing.T) {
	handlers = nil

	err := Shutdown(context.Background())
	if err != nil {
		t.Errorf("Shutdown() with no handlers error = %v, want nil", err)
	}
}

func TestWaitForSignalWithTimeout_HandlerReceivesValidContext(t *testing.T) {
	handlers = nil

	var ctxErr error
	var hasDeadline bool
	Register(func(ctx context.Context) error {
		ctxErr = ctx.Err()
		_, hasDeadline = ctx.Deadline()
		return nil
	})

	// Cancel parent context to trigger shutdown
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := WaitForSignalWithTimeout(ctx, 5*time.Second)
	if err != nil {
		t.Errorf("WaitForSignalWithTimeout() error = %v, want nil", err)
	}

	// Handler should receive a non-cancelled context with deadline
	if ctxErr != nil {
		t.Errorf("Handler received cancelled context: %v", ctxErr)
	}
	if !hasDeadline {
		t.Error("Handler context should have deadline from timeout")
	}
}

func TestWaitForSignalWithTimeout_RespectsTimeout(t *testing.T) {
	handlers = nil

	timeout := 100 * time.Millisecond
	var deadline time.Time
	var hasDeadline bool

	Register(func(ctx context.Context) error {
		deadline, hasDeadline = ctx.Deadline()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_ = WaitForSignalWithTimeout(ctx, timeout)

	if !hasDeadline {
		t.Fatal("Handler context should have deadline")
	}

	expectedDeadline := start.Add(timeout)
	if deadline.Before(start) || deadline.After(expectedDeadline.Add(50*time.Millisecond)) {
		t.Errorf("Deadline %v not within expected range around %v", deadline, expectedDeadline)
	}
}

func TestWaitForSignal_UsesDefaultTimeout(t *testing.T) {
	handlers = nil

	var hasDeadline bool
	Register(func(ctx context.Context) error {
		_, hasDeadline = ctx.Deadline()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = WaitForSignal(ctx)

	if !hasDeadline {
		t.Error("WaitForSignal should use DefaultShutdownTimeout")
	}
}

func TestDefaultShutdownTimeout(t *testing.T) {
	if DefaultShutdownTimeout != 30*time.Second {
		t.Errorf("DefaultShutdownTimeout = %v, want 30s", DefaultShutdownTimeout)
	}
}
