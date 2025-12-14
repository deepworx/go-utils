package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const meterName = "github.com/deepworx/go-utils/pkg/postgres"

func registerMetrics(pool *pgxpool.Pool) error {
	meter := otel.Meter(meterName)

	_, err := meter.Int64ObservableGauge(
		"db.pool.total_conns",
		metric.WithDescription("Total number of connections in the pool"),
		metric.WithUnit("{connection}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(pool.Stat().TotalConns()))
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("register total_conns metric: %w", err)
	}

	_, err = meter.Int64ObservableGauge(
		"db.pool.idle_conns",
		metric.WithDescription("Number of idle connections in the pool"),
		metric.WithUnit("{connection}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(pool.Stat().IdleConns()))
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("register idle_conns metric: %w", err)
	}

	_, err = meter.Int64ObservableGauge(
		"db.pool.acquired_conns",
		metric.WithDescription("Number of acquired connections in use"),
		metric.WithUnit("{connection}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(pool.Stat().AcquiredConns()))
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("register acquired_conns metric: %w", err)
	}

	_, err = meter.Int64ObservableGauge(
		"db.pool.max_conns",
		metric.WithDescription("Maximum configured connections"),
		metric.WithUnit("{connection}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(pool.Stat().MaxConns()))
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("register max_conns metric: %w", err)
	}

	return nil
}
