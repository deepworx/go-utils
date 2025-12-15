package postgres

import (
	"context"
	"testing"
)

func TestNewHealthChecker(t *testing.T) {
	t.Parallel()

	// NewHealthChecker should accept nil pool (validation happens at Check time)
	checker := NewHealthChecker(nil)
	if checker == nil {
		t.Error("NewHealthChecker returned nil")
	}
}

func TestHealthChecker_ImplementsInterface(t *testing.T) {
	t.Parallel()

	// Verify HealthChecker has the expected method signature.
	// This is a compile-time check that the type satisfies grpchealth.HealthChecker.
	var _ interface {
		Check(ctx context.Context) bool
	} = (*HealthChecker)(nil)
}
