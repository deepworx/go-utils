package eventstore

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/internal/telemetry"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
)

const scope = "github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"

// RepositoryOption configures a Repository.
type RepositoryOption func(*repositoryOptions)

type repositoryOptions struct {
	providers telemetry.Providers
}

// WithTracerProvider records Command spans with tp instead of the global
// provider.
func WithTracerProvider(tp trace.TracerProvider) RepositoryOption {
	return func(o *repositoryOptions) { o.providers.TracerProvider = tp }
}

// WithMeterProvider records the load and Command histograms with mp instead
// of the global provider.
func WithMeterProvider(mp metric.MeterProvider) RepositoryOption {
	return func(o *repositoryOptions) { o.providers.MeterProvider = mp }
}

// instruments records the spans and histograms of one Repository. Metric
// attributes are limited to the stream type, and whether a Command was
// replayed.
type instruments struct {
	tracer          trace.Tracer
	streamType      string
	labels          metric.MeasurementOption
	commandLabels   metric.MeasurementOption
	replayedLabels  metric.MeasurementOption
	loadDuration    metric.Float64Histogram
	loadEvents      metric.Int64Histogram
	commandDuration metric.Float64Histogram
}

// newInstruments creates the instruments for streamType. An instrument
// that fails to create is replaced by a no-op and the error goes to
// otel.Handle.
func newInstruments(providers telemetry.Providers, streamType string) instruments {
	meter := providers.Meter(scope)
	in := instruments{
		tracer:          providers.Tracer(scope),
		streamType:      streamType,
		labels:          metric.WithAttributeSet(attribute.NewSet(telemetry.AttrStreamType.String(streamType))),
		commandLabels:   commandLabels(streamType, false),
		replayedLabels:  commandLabels(streamType, true),
		loadDuration:    noop.Float64Histogram{},
		loadEvents:      noop.Int64Histogram{},
		commandDuration: noop.Float64Histogram{},
	}
	var errs []error
	if h, err := telemetry.AggregateLoadDuration.Float64Histogram(meter); err != nil {
		errs = append(errs, err)
	} else {
		in.loadDuration = h
	}
	if h, err := telemetry.AggregateLoadEvents.Int64Histogram(meter); err != nil {
		errs = append(errs, err)
	} else {
		in.loadEvents = h
	}
	if h, err := telemetry.CommandDuration.Float64Histogram(meter); err != nil {
		errs = append(errs, err)
	} else {
		in.commandDuration = h
	}
	if err := errors.Join(errs...); err != nil {
		otel.Handle(err)
	}
	return in
}

func commandLabels(streamType string, replayed bool) metric.MeasurementOption {
	return metric.WithAttributeSet(attribute.NewSet(
		telemetry.AttrStreamType.String(streamType),
		telemetry.AttrCommandReplayed.Bool(replayed),
	))
}

type commandInput struct {
	// name is the span name, SpanCreate or SpanUpdate.
	name   string
	tenant string
	stream string
}

// command runs fn in an INTERNAL Command span and records its duration. A
// replayed result marks the span.
func (in instruments) command(ctx context.Context, cmd commandInput, fn func(ctx context.Context) (CommandResult, error)) (CommandResult, error) {
	start := time.Now()
	ctx, span := in.tracer.Start(ctx, cmd.name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			telemetry.AttrStreamType.String(in.streamType),
			telemetry.AttrTenantID.String(cmd.tenant),
			telemetry.AttrStreamID.String(cmd.stream),
		),
	)
	defer span.End()
	res, err := fn(ctx)
	labels := in.commandLabels
	if res.Replayed {
		labels = in.replayedLabels
		span.SetAttributes(telemetry.AttrCommandReplayed.Bool(true))
	}
	in.commandDuration.Record(ctx, time.Since(start).Seconds(), labels)
	endWithError(span, err)
	return res, err
}

// raising annotates the Command span in ctx with the types of the events
// it is about to append, deduplicated in raise order.
func raising(ctx context.Context, events []aggregate.Event) {
	types := make([]string, 0, len(events))
	seen := make(map[string]bool, len(events))
	for _, e := range events {
		if t := e.EventType(); !seen[t] {
			seen[t] = true
			types = append(types, t)
		}
	}
	trace.SpanFromContext(ctx).SetAttributes(telemetry.AttrEventTypes.StringSlice(types))
}

// send runs fn in the PRODUCER span of an append and passes it the span's
// traceparent, which is stored on the events.
func (in instruments) send(ctx context.Context, fn func(ctx context.Context, traceContext string) error) error {
	ctx, span := in.tracer.Start(ctx, telemetry.SpanSend(in.streamType),
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(telemetry.AttrStreamType.String(in.streamType)),
	)
	defer span.End()
	err := fn(ctx, traceParent(ctx))
	endWithError(span, err)
	return err
}

// load runs fn, which loads an Aggregate and returns the number of events
// it read, and records the load histograms.
func (in instruments) load(ctx context.Context, fn func() (int, error)) error {
	start := time.Now()
	n, err := fn()
	in.loadDuration.Record(ctx, time.Since(start).Seconds(), in.labels)
	if err == nil {
		in.loadEvents.Record(ctx, int64(n), in.labels)
	}
	return err
}

// traceParent returns the W3C traceparent of the span in ctx, or "" when
// it has none. TraceContext never carries baggage, and its tracestate
// header is dropped.
func traceParent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

func endWithError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	span.SetAttributes(telemetry.AttrErrorType.String(errorType(err)))
}

// errorTypes are the low-cardinality error.type values of the package's
// sentinels.
var errorTypes = []struct {
	err  error
	name string
}{
	{ErrConcurrencyConflict, "concurrency_conflict"},
	{ErrCommandIDReused, "command_id_reused"},
	{ErrAlreadyExists, "already_exists"},
	{ErrNotFound, "not_found"},
	{ErrStreamMismatch, "stream_mismatch"},
	{ErrNestedUnitOfWork, "nested_unit_of_work"},
	{ErrNotCommandTransaction, "not_command_transaction"},
	{ErrNoTenant, "no_tenant"},
	{ErrNoActor, "no_actor"},
	{ErrUnknownEventType, "unknown_event_type"},
	{ErrNewerSchemaVersion, "newer_schema_version"},
	{ErrInvalidSchemaVersion, "invalid_schema_version"},
	{context.Canceled, "canceled"},
	{context.DeadlineExceeded, "deadline_exceeded"},
}

// errorType returns the error.type of err: a sentinel's name, or _OTHER as
// the OTel semantic conventions prescribe for anything else.
func errorType(err error) string {
	for _, t := range errorTypes {
		if errors.Is(err, t.err) {
			return t.name
		}
	}
	return "_OTHER"
}
