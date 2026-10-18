package consumer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

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

// xminGauges holds one eventsourcing.xmin.age callback per Meter, so the
// PgRuntimes of one process report a single series.
var xminGauges = xminRegistry{byMeter: make(map[metric.Meter]*xminGauge)}

type xminRegistry struct {
	mu      sync.Mutex
	byMeter map[metric.Meter]*xminGauge
}

// xminGauge is the callback of one Meter. It polls the pool of the oldest
// open PgRuntime; xmin is cluster-wide.
type xminGauge struct {
	reg   metric.Registration
	pools []*pgxpool.Pool
}

// add reports eventsourcing.xmin.age through meter, polled on pool, until
// the returned remove is called for every add. Errors go to otel.Handle.
func (r *xminRegistry) add(meter metric.Meter, pool *pgxpool.Pool) (remove func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.byMeter[meter]
	if g == nil {
		var err error
		if g, err = r.register(meter); err != nil {
			otel.Handle(fmt.Errorf("register xmin age gauge: %w", err))
			return func() {}
		}
		r.byMeter[meter] = g
	}
	g.pools = append(g.pools, pool)
	var once sync.Once
	return func() { once.Do(func() { r.remove(meter, pool) }) }
}

// register creates the callback of meter. The caller holds r.mu.
func (r *xminRegistry) register(meter metric.Meter) (*xminGauge, error) {
	gauge, err := telemetry.XminAge.Int64ObservableGauge(meter)
	if err != nil {
		return nil, err
	}
	g := &xminGauge{}
	g.reg, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		r.mu.Lock()
		if len(g.pools) == 0 {
			r.mu.Unlock()
			return nil
		}
		pool := g.pools[0]
		r.mu.Unlock()
		var age int64
		if err := pool.QueryRow(ctx, xminAgeQuery).Scan(&age); err != nil {
			return fmt.Errorf("poll xmin age: %w", err)
		}
		o.ObserveInt64(gauge, age)
		return nil
	}, gauge)
	if err != nil {
		return nil, err
	}
	return g, nil
}

func (r *xminRegistry) remove(meter metric.Meter, pool *pgxpool.Pool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.byMeter[meter]
	if g == nil {
		return
	}
	if i := slices.Index(g.pools, pool); i >= 0 {
		g.pools = slices.Delete(g.pools, i, i+1)
	}
	if len(g.pools) > 0 {
		return
	}
	delete(r.byMeter, meter)
	if err := g.reg.Unregister(); err != nil {
		otel.Handle(fmt.Errorf("unregister xmin age gauge: %w", err))
	}
}
