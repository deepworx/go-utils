package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HealthChecker checks PostgreSQL pool connectivity.
// Implements grpchealth.HealthChecker interface.
type HealthChecker struct {
	pool *pgxpool.Pool
}

// NewHealthChecker creates a health checker for the given pool.
func NewHealthChecker(pool *pgxpool.Pool) *HealthChecker {
	return &HealthChecker{pool: pool}
}

// Check returns true if the database is reachable.
func (c *HealthChecker) Check(ctx context.Context) bool {
	return c.pool.Ping(ctx) == nil
}
