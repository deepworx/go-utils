// Package telemetry is the single registry of span names, metric
// instruments, attribute keys, log keys and histogram boundaries used by the
// event-sourcing, scheduler, inbox, notify and lease packages.
//
// The instrumentation scope name is always the import path of the package
// that records. Providers default to the OTel globals, so everything is a
// no-op until otel.Setup installs real providers.
package telemetry

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Providers holds the providers a package records with. A nil field falls
// back to the corresponding OTel global, resolved on every call. Public
// packages back their WithTracerProvider/WithMeterProvider options with it.
type Providers struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
}

// Tracer returns the tracer for scope, which must be the caller's package
// import path.
func (p Providers) Tracer(scope string) trace.Tracer {
	if p.TracerProvider == nil {
		return otel.GetTracerProvider().Tracer(scope)
	}
	return p.TracerProvider.Tracer(scope)
}

// Meter returns the meter for scope, which must be the caller's package
// import path.
func (p Providers) Meter(scope string) metric.Meter {
	if p.MeterProvider == nil {
		return otel.GetMeterProvider().Meter(scope)
	}
	return p.MeterProvider.Meter(scope)
}

// Instrument describes one metric instrument. Its methods create the
// instrument on a meter with the registered name, unit, description and,
// for histograms, explicit bucket boundaries. The boundaries only apply when
// the SDK is configured for explicit histograms.
type Instrument struct {
	Name        string
	Unit        string
	Description string
	Boundaries  []float64
}

// Float64Histogram creates the instrument as a float64 histogram.
func (i Instrument) Float64Histogram(m metric.Meter) (metric.Float64Histogram, error) {
	return m.Float64Histogram(i.Name,
		metric.WithUnit(i.Unit),
		metric.WithDescription(i.Description),
		metric.WithExplicitBucketBoundaries(i.Boundaries...),
	)
}

// Int64Histogram creates the instrument as an int64 histogram.
func (i Instrument) Int64Histogram(m metric.Meter) (metric.Int64Histogram, error) {
	return m.Int64Histogram(i.Name,
		metric.WithUnit(i.Unit),
		metric.WithDescription(i.Description),
		metric.WithExplicitBucketBoundaries(i.Boundaries...),
	)
}

// Int64Counter creates the instrument as an int64 counter.
func (i Instrument) Int64Counter(m metric.Meter) (metric.Int64Counter, error) {
	return m.Int64Counter(i.Name, metric.WithUnit(i.Unit), metric.WithDescription(i.Description))
}

// Int64UpDownCounter creates the instrument as an int64 up-down counter.
func (i Instrument) Int64UpDownCounter(m metric.Meter) (metric.Int64UpDownCounter, error) {
	return m.Int64UpDownCounter(i.Name, metric.WithUnit(i.Unit), metric.WithDescription(i.Description))
}

// Int64ObservableGauge creates the instrument as an asynchronous int64
// gauge. An owner-only gauge registers its callback only while the lock or
// lease is held, so a pod that loses it stops reporting the series.
func (i Instrument) Int64ObservableGauge(m metric.Meter, opts ...metric.Int64ObservableGaugeOption) (metric.Int64ObservableGauge, error) {
	opts = append([]metric.Int64ObservableGaugeOption{metric.WithUnit(i.Unit), metric.WithDescription(i.Description)}, opts...)
	return m.Int64ObservableGauge(i.Name, opts...)
}

// Float64ObservableGauge creates the instrument as an asynchronous float64
// gauge. An owner-only gauge registers its callback only while the lock or
// lease is held, so a pod that loses it stops reporting the series.
func (i Instrument) Float64ObservableGauge(m metric.Meter, opts ...metric.Float64ObservableGaugeOption) (metric.Float64ObservableGauge, error) {
	opts = append([]metric.Float64ObservableGaugeOption{metric.WithUnit(i.Unit), metric.WithDescription(i.Description)}, opts...)
	return m.Float64ObservableGauge(i.Name, opts...)
}
