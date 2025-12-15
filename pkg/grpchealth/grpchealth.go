// Package grpchealth provides a health check aggregator for connectrpc.com/grpchealth.
//
// It aggregates multiple health checkers and updates a gRPC health endpoint based on
// their combined status. All registered checkers are probed in parallel at configurable
// intervals, and the aggregate status is set to serving only if all checks pass.
package grpchealth

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
)

// HealthChecker checks the readiness of a service.
type HealthChecker interface {
	// Check returns true if the service is ready.
	// The context contains the configured timeout.
	Check(ctx context.Context) bool
}

// HealthCheckerFunc allows simple functions to be used as HealthChecker.
type HealthCheckerFunc func(ctx context.Context) bool

// Check implements HealthChecker.
func (f HealthCheckerFunc) Check(ctx context.Context) bool {
	return f(ctx)
}

// Config holds configuration for the health aggregator.
type Config struct {
	// Interval between health check cycles.
	Interval time.Duration `koanf:"interval"`

	// Timeout for each individual health check.
	Timeout time.Duration `koanf:"timeout"`
}

// DefaultConfig returns a Config with sensible default values.
func DefaultConfig() Config {
	return Config{
		Interval: 10 * time.Second,
		Timeout:  5 * time.Second,
	}
}

// Aggregator probes registered health checkers and updates gRPC health status.
type Aggregator struct {
	cfg     Config
	checker *grpchealth.StaticChecker

	mu       sync.RWMutex
	services map[string]HealthChecker
	serving  bool
}

// NewAggregator creates a new health aggregator.
// The aggregator starts in NotServing state until the first check cycle completes.
func NewAggregator(cfg Config) *Aggregator {
	checker := grpchealth.NewStaticChecker()
	checker.SetStatus("", grpchealth.StatusNotServing)

	return &Aggregator{
		cfg:      cfg,
		checker:  checker,
		services: make(map[string]HealthChecker),
		serving:  false,
	}
}

// Register adds a health checker with the given name.
// Returns the Aggregator for method chaining.
// Panics if name is empty or already registered.
func (a *Aggregator) Register(name string, checker HealthChecker) *Aggregator {
	if name == "" {
		panic("grpchealth: name cannot be empty")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.services[name]; exists {
		panic("grpchealth: checker already registered: " + name)
	}

	a.services[name] = checker
	return a
}

// Handler returns the HTTP handler for the gRPC health endpoint.
// Mount on your HTTP mux: mux.Handle(aggregator.Handler())
func (a *Aggregator) Handler(opts ...connect.HandlerOption) (string, http.Handler) {
	return grpchealth.NewHandler(a.checker, opts...)
}

// Run starts the health check loop and blocks until ctx is cancelled.
// It probes all registered checkers in parallel and updates the aggregate status.
func (a *Aggregator) Run(ctx context.Context) error {
	ticker := time.NewTicker(a.cfg.Interval)
	defer ticker.Stop()

	// Run first check immediately
	a.runChecks(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			a.runChecks(ctx)
		}
	}
}

// IsServing returns the current aggregate health status (thread-safe).
func (a *Aggregator) IsServing() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.serving
}

// runChecks executes all registered health checks in parallel.
func (a *Aggregator) runChecks(ctx context.Context) {
	a.mu.RLock()
	services := make(map[string]HealthChecker, len(a.services))
	for name, checker := range a.services {
		services[name] = checker
	}
	a.mu.RUnlock()

	if len(services) == 0 {
		a.updateStatus(true, nil)
		return
	}

	results := make(map[string]bool, len(services))
	var resultsMu sync.Mutex
	var wg sync.WaitGroup

	for name, checker := range services {
		wg.Add(1)
		go func(name string, checker HealthChecker) {
			defer wg.Done()

			checkCtx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
			defer cancel()

			healthy := a.safeCheck(checkCtx, name, checker)

			resultsMu.Lock()
			results[name] = healthy
			resultsMu.Unlock()
		}(name, checker)
	}

	wg.Wait()

	allHealthy := true
	for _, healthy := range results {
		if !healthy {
			allHealthy = false
			break
		}
	}

	a.updateStatus(allHealthy, results)
}

// safeCheck executes a health check with panic recovery.
func (a *Aggregator) safeCheck(ctx context.Context, name string, checker HealthChecker) (healthy bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("health check panicked",
				"service", name,
				"panic", r,
			)
			healthy = false
		}
	}()

	return checker.Check(ctx)
}

// updateStatus updates the aggregate status and logs changes.
func (a *Aggregator) updateStatus(serving bool, results map[string]bool) {
	a.mu.Lock()
	changed := a.serving != serving
	a.serving = serving
	a.mu.Unlock()

	if serving {
		a.checker.SetStatus("", grpchealth.StatusServing)
	} else {
		a.checker.SetStatus("", grpchealth.StatusNotServing)
	}

	if changed {
		attrs := []any{
			"serving", serving,
		}
		if results != nil {
			attrs = append(attrs, "checks", results)
		}
		slog.Info("health status changed", attrs...)
	}
}
