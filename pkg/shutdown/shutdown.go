// Package shutdown provides graceful shutdown orchestration for services.
package shutdown

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// DefaultShutdownTimeout is the default time allowed for graceful shutdown.
const DefaultShutdownTimeout = 30 * time.Second

// Handler is called during shutdown with the provided context.
type Handler func(ctx context.Context) error

var (
	mu       sync.Mutex
	handlers []Handler
)

// Register adds a shutdown handler. Handlers are called in LIFO order.
func Register(h Handler) {
	mu.Lock()
	defer mu.Unlock()
	handlers = append(handlers, h)
}

// Shutdown executes all registered handlers in LIFO order.
// Returns a combined error if any handler fails.
func Shutdown(ctx context.Context) error {
	mu.Lock()
	defer mu.Unlock()

	var errs []error
	for i := len(handlers) - 1; i >= 0; i-- {
		if err := handlers[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	handlers = nil
	return errors.Join(errs...)
}

// WaitForSignal blocks until SIGINT or SIGTERM is received, then calls Shutdown
// with DefaultShutdownTimeout.
func WaitForSignal(ctx context.Context) error {
	return WaitForSignalWithTimeout(ctx, DefaultShutdownTimeout)
}

// WaitForSignalWithTimeout blocks until SIGINT or SIGTERM is received, then calls
// Shutdown with the specified timeout for handlers to complete.
func WaitForSignalWithTimeout(ctx context.Context, timeout time.Duration) error {
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-sigCtx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return Shutdown(shutdownCtx)
}
