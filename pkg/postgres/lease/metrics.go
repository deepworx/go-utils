package lease

import (
	"errors"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/deepworx/go-utils/internal/telemetry"
)

type metrics struct {
	reconnects metric.Int64Counter
	lost       metric.Int64Counter
	held       metric.Int64UpDownCounter
}

// newMetrics creates the instruments on meter. Instruments that fail to
// create fall back to no-ops, so the returned metrics are always usable.
func newMetrics(meter metric.Meter) (metrics, error) {
	reconnects, errReconnects := telemetry.LeaseReconnects.Int64Counter(meter)
	if errReconnects != nil {
		reconnects = noop.Int64Counter{}
	}
	lost, errLost := telemetry.LeaseLost.Int64Counter(meter)
	if errLost != nil {
		lost = noop.Int64Counter{}
	}
	held, errHeld := telemetry.LeaseHeld.Int64UpDownCounter(meter)
	if errHeld != nil {
		held = noop.Int64UpDownCounter{}
	}
	return metrics{reconnects: reconnects, lost: lost, held: held}, errors.Join(errReconnects, errLost, errHeld)
}
