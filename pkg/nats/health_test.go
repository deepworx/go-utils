package nats

import (
	"testing"
)

func TestNewHealthChecker(t *testing.T) {
	t.Parallel()

	checker := NewHealthChecker(nil)
	if checker == nil {
		t.Fatal("NewHealthChecker() returned nil")
	}
	if checker.conn != nil {
		t.Error("expected nil connection")
	}
}

func TestHealthChecker_Check_NilConn(t *testing.T) {
	t.Parallel()

	checker := NewHealthChecker(nil)

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic with nil connection")
		}
	}()

	checker.Check(t.Context())
}
