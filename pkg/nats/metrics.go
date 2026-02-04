package nats

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const meterName = "github.com/deepworx/go-utils/pkg/nats"

// registerMetrics registers NATS connection metrics with the global OTel meter.
func registerMetrics(conn *nats.Conn) error {
	meter := otel.Meter(meterName)

	_, err := meter.Int64ObservableGauge(
		"nats.connection.status",
		metric.WithDescription("NATS connection status (0=disconnected, 1=connected, 2=reconnecting, 3=closed)"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(conn.Status()))
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("register connection.status metric: %w", err)
	}

	_, err = meter.Int64ObservableCounter(
		"nats.reconnects.total",
		metric.WithDescription("Total number of reconnections"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(conn.Stats().Reconnects))
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("register reconnects.total metric: %w", err)
	}

	_, err = meter.Int64ObservableCounter(
		"nats.messages.in.total",
		metric.WithDescription("Total messages received"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(conn.Stats().InMsgs))
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("register messages.in.total metric: %w", err)
	}

	_, err = meter.Int64ObservableCounter(
		"nats.messages.out.total",
		metric.WithDescription("Total messages sent"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(conn.Stats().OutMsgs))
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("register messages.out.total metric: %w", err)
	}

	_, err = meter.Int64ObservableCounter(
		"nats.bytes.in.total",
		metric.WithDescription("Total bytes received"),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(conn.Stats().InBytes))
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("register bytes.in.total metric: %w", err)
	}

	_, err = meter.Int64ObservableCounter(
		"nats.bytes.out.total",
		metric.WithDescription("Total bytes sent"),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(conn.Stats().OutBytes))
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("register bytes.out.total metric: %w", err)
	}

	return nil
}
