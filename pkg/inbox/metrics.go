package inbox

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/deepworx/go-utils/internal/telemetry"
)

type metrics struct {
	handleDuration metric.Float64Histogram
	handled        metric.Int64Counter

	// meter, messages and oldestAge are the owner-only gauges; gauges is
	// false when they could not be created.
	meter     metric.Meter
	messages  metric.Int64ObservableGauge
	oldestAge metric.Float64ObservableGauge
	gauges    bool
}

// newMetrics creates the Worker's instruments. Instruments that fail to
// create are replaced by no-ops and reported in the returned error.
func newMetrics(meter metric.Meter) (metrics, error) {
	var errs []error
	m := metrics{
		handleDuration: noop.Float64Histogram{},
		handled:        noop.Int64Counter{},
		meter:          meter,
	}
	if h, err := telemetry.InboxHandleDuration.Float64Histogram(meter); err != nil {
		errs = append(errs, err)
	} else {
		m.handleDuration = h
	}
	if c, err := telemetry.InboxMessagesHandled.Int64Counter(meter); err != nil {
		errs = append(errs, err)
	} else {
		m.handled = c
	}
	messages, errMessages := telemetry.InboxMessages.Int64ObservableGauge(meter)
	oldestAge, errOldest := telemetry.InboxPendingOldestAge.Float64ObservableGauge(meter)
	if errMessages != nil || errOldest != nil {
		errs = append(errs, errMessages, errOldest)
	} else {
		m.messages, m.oldestAge, m.gauges = messages, oldestAge, true
	}
	return m, errors.Join(errs...)
}

// observeInput is what the owner-only gauges report.
type observeInput struct {
	source string
	counts func(ctx context.Context) (sourceCounts, error)
}

// observe reports the gauges of in.source from in.counts at each
// collection until the returned stop is called. Only the telemetry lease
// owner calls it, so a pod that loses the lease stops exporting the series.
func (m metrics) observe(in observeInput) (stop func()) {
	if !m.gauges {
		return func() {}
	}
	source := telemetry.AttrSource.String(in.source)
	reg, err := m.meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		counts, err := in.counts(ctx)
		if err != nil {
			return fmt.Errorf("inbox: poll gauges of %s: %w", in.source, err)
		}
		for status, n := range counts.ByStatus {
			o.ObserveInt64(m.messages, n, metric.WithAttributes(source, telemetry.AttrStatus.String(string(status))))
		}
		o.ObserveFloat64(m.oldestAge, counts.OldestPending.Seconds(), metric.WithAttributes(source))
		return nil
	}, m.messages, m.oldestAge)
	if err != nil {
		otel.Handle(fmt.Errorf("inbox: register gauges of %s: %w", in.source, err))
		return func() {}
	}
	return func() {
		if err := reg.Unregister(); err != nil {
			otel.Handle(fmt.Errorf("inbox: unregister gauges of %s: %w", in.source, err))
		}
	}
}
