package nats

import (
	"context"

	"github.com/nats-io/nats.go"
)

// HealthChecker checks NATS connection status.
// Implements grpchealth.HealthChecker interface.
type HealthChecker struct {
	conn *nats.Conn
}

// NewHealthChecker creates a health checker for the given connection.
func NewHealthChecker(conn *nats.Conn) *HealthChecker {
	return &HealthChecker{conn: conn}
}

// Check returns true if the connection is active.
func (c *HealthChecker) Check(_ context.Context) bool {
	return c.conn.Status() == nats.CONNECTED
}
