package inbox

import (
	"errors"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/deepworx/go-utils/internal/telemetry"
)

type metrics struct {
	handleDuration metric.Float64Histogram
	handled        metric.Int64Counter
	messages       metric.Int64Gauge
	oldestAge      metric.Float64Gauge
}

// newMetrics creates the Worker's instruments. Instruments that fail to
// create are replaced by no-ops and reported in the returned error.
func newMetrics(meter metric.Meter) (metrics, error) {
	var errs []error
	m := metrics{
		handleDuration: noop.Float64Histogram{},
		handled:        noop.Int64Counter{},
		messages:       noop.Int64Gauge{},
		oldestAge:      noop.Float64Gauge{},
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
	if g, err := telemetry.InboxMessages.Int64Gauge(meter); err != nil {
		errs = append(errs, err)
	} else {
		m.messages = g
	}
	if g, err := telemetry.InboxPendingOldestAge.Float64Gauge(meter); err != nil {
		errs = append(errs, err)
	} else {
		m.oldestAge = g
	}
	return m, errors.Join(errs...)
}
