package consumer

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/internal/telemetry"
)

// links returns span links to the stored trace contexts of up to
// telemetry.MaxLinks records. Events without a valid context are skipped.
func links(records []record) []trace.Link {
	var out []trace.Link
	for _, r := range records {
		if len(out) == telemetry.MaxLinks {
			break
		}
		if r.traceContext == "" {
			continue
		}
		ctx := propagation.TraceContext{}.Extract(context.Background(),
			propagation.MapCarrier{"traceparent": r.traceContext})
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			out = append(out, trace.Link{SpanContext: sc})
		}
	}
	return out
}

// instruments are a Consumer's histograms and polled gauges. Their only
// label is the Consumer's name.
type instruments struct {
	meter           metric.Meter
	labels          metric.MeasurementOption
	batchDuration   metric.Float64Histogram
	processDuration metric.Float64Histogram
	pending         metric.Int64ObservableGauge
	lag             metric.Float64ObservableGauge
	stopped         metric.Int64ObservableGauge
}

// newInstruments creates the instruments of the Consumer name. An
// instrument that fails to create is replaced by a no-op and the error goes
// to otel.Handle.
func newInstruments(meter metric.Meter, name string) instruments {
	in := instruments{
		meter:           meter,
		labels:          metric.WithAttributeSet(attribute.NewSet(telemetry.AttrConsumer.String(name))),
		batchDuration:   noop.Float64Histogram{},
		processDuration: noop.Float64Histogram{},
		pending:         noop.Int64ObservableGauge{},
		lag:             noop.Float64ObservableGauge{},
		stopped:         noop.Int64ObservableGauge{},
	}
	var errs []error
	keep(&errs, &in.batchDuration)(telemetry.ConsumerBatchDuration.Float64Histogram(meter))
	keep(&errs, &in.processDuration)(telemetry.ConsumerProcessDuration.Float64Histogram(meter))
	keep(&errs, &in.pending)(telemetry.ConsumerPending.Int64ObservableGauge(meter))
	keep(&errs, &in.lag)(telemetry.ConsumerLag.Float64ObservableGauge(meter))
	keep(&errs, &in.stopped)(telemetry.ConsumerStopped.Int64ObservableGauge(meter))
	if err := errors.Join(errs...); err != nil {
		otel.Handle(err)
	}
	return in
}

// keep returns a function that stores a created instrument in *dst, or
// leaves the no-op in *dst and collects the creation error in errs.
func keep[T any](errs *[]error, dst *T) func(T, error) {
	return func(inst T, err error) {
		if err != nil {
			*errs = append(*errs, err)
			return
		}
		*dst = inst
	}
}

// reportGauges reports the pending, lag and stopped gauges from status at
// each collection until the returned stop is called. Only the lease owner
// calls it, so each series has exactly one writer.
func (in instruments) reportGauges(status func(context.Context) (Status, error)) (stop func()) {
	reg, err := in.meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		s, err := status(ctx)
		if err != nil {
			return fmt.Errorf("poll consumer gauges: %w", err)
		}
		var stopped int64
		if s.Stopped != nil {
			stopped = 1
		}
		o.ObserveInt64(in.pending, int64(s.Pending), in.labels)
		o.ObserveFloat64(in.lag, s.Age.Seconds(), in.labels)
		o.ObserveInt64(in.stopped, stopped, in.labels)
		return nil
	}, in.pending, in.lag, in.stopped)
	if err != nil {
		otel.Handle(fmt.Errorf("register consumer gauges: %w", err))
		return func() {}
	}
	return func() {
		if err := reg.Unregister(); err != nil {
			otel.Handle(fmt.Errorf("unregister consumer gauges: %w", err))
		}
	}
}

// reportXminAge reports eventsourcing.xmin.age from age at each
// collection, once per Runtime and therefore once per process. Errors go
// to otel.Handle.
func reportXminAge(meter metric.Meter, age func(context.Context) (int64, error)) {
	_, err := telemetry.XminAge.Int64ObservableGauge(meter,
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			a, err := age(ctx)
			if err != nil {
				return fmt.Errorf("poll xmin age: %w", err)
			}
			o.Observe(a)
			return nil
		}))
	if err != nil {
		otel.Handle(fmt.Errorf("register xmin age gauge: %w", err))
	}
}
