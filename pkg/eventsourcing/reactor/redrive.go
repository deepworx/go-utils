package reactor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/deepworx/go-utils/internal/telemetry"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
)

const scope = "github.com/deepworx/go-utils/pkg/eventsourcing/reactor"

// maxRedriveWait bounds how long the auto-redrive loop sleeps, so a hold
// parked by an earlier lease holder is redriven even without a wake-up.
const maxRedriveWait = time.Minute

// Status is the consumer Status of a Reactor plus its park list.
type Status struct {
	consumer.Status
	// ParkedStreams counts the Parked Streams.
	ParkedStreams int
	// ParkedEvents counts the events held for them.
	ParkedEvents int
	// Exhausted counts the Parked Streams whose auto-redrive schedule is
	// spent.
	Exhausted int
}

// Status returns the Reactor's consumer Status and the size of its park
// list.
func (r *Reactor) Status(ctx context.Context) (Status, error) {
	cs, err := r.consumer.Status(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("reactor status: %w", err)
	}
	ps, err := r.parks.stats(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("reactor %q status: park list: %w", r.name, err)
	}
	return Status{Status: cs, ParkedStreams: ps.streams, ParkedEvents: ps.events, Exhausted: ps.exhausted}, nil
}

// Drain processes events up to the head of the log like consumer.Drain,
// then runs the auto-redrives that are due.
func (r *Reactor) Drain(ctx context.Context) error {
	return r.consumer.Drain(ctx)
}

// leased runs while the Reactor's Consumer holds the lease: it reports the
// park list gauges and runs auto-redrive until stop is called or ctx, the
// lease ctx, ends.
func (r *Reactor) leased(ctx context.Context) (stop func()) {
	stopGauges := r.inst.report(r.parks.stats)
	if len(r.opts.redrive) == 0 {
		return stopGauges
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { r.autoRedriveLoop(ctx) })
	return func() {
		cancel()
		wg.Wait()
		stopGauges()
	}
}

// autoRedriveLoop runs the due auto-redrives whenever the earliest one
// comes due or a Stream is Parked, until ctx ends.
func (r *Reactor) autoRedriveLoop(ctx context.Context) {
	for {
		if err := r.redriveDue(ctx); err != nil && ctx.Err() == nil {
			r.logger.ErrorContext(ctx, "auto-redrive", telemetry.LogError, err)
		}
		timer := time.NewTimer(r.untilNextDue(ctx))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.parked:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (r *Reactor) untilNextDue(ctx context.Context) time.Duration {
	next, ok, err := r.parks.nextDue(ctx)
	if err != nil || !ok {
		return maxRedriveWait
	}
	return min(max(next.Sub(r.now()), 0), maxRedriveWait)
}

// redriveDue auto-redrives every Stream that is due now. A failing
// reaction moves that Stream's schedule on and is logged; only park list
// errors are returned.
func (r *Reactor) redriveDue(ctx context.Context) error {
	if len(r.opts.redrive) == 0 {
		return nil
	}
	auto := &autoRedrive{now: r.now(), steps: r.opts.redrive}
	streams, err := r.parks.due(ctx, auto.now)
	if err != nil {
		return fmt.Errorf("list due redrives: %w", err)
	}
	var errs []error
	for _, s := range streams {
		res, failure, err := r.redrive(ctx, redriveInput{stream: s, replay: r.react, auto: auto})
		switch {
		case errors.Is(err, ErrNotParked):
		case err != nil:
			errs = append(errs, fmt.Errorf("auto-redrive %s/%s: %w", s.Tenant, s.ID, err))
		case failure != nil:
			r.logger.WarnContext(ctx, "auto-redrive failed",
				telemetry.LogTenantID, s.Tenant, telemetry.LogStreamID, s.ID,
				"replayed", res.Replayed, "remaining", res.Remaining, telemetry.LogError, failure)
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
	}
	return errors.Join(errs...)
}

// instruments are the Reactor's park list gauges, reported only by the
// lease owner. Their only label is the Reactor's name.
type instruments struct {
	meter     metric.Meter
	labels    metric.MeasurementOption
	streams   metric.Int64ObservableGauge
	events    metric.Int64ObservableGauge
	exhausted metric.Int64ObservableGauge
	err       error
}

func newInstruments(meter metric.Meter, name string) instruments {
	in := instruments{
		meter:  meter,
		labels: metric.WithAttributeSet(attribute.NewSet(telemetry.AttrConsumer.String(name))),
	}
	var errs [3]error
	in.streams, errs[0] = telemetry.ReactorParkedStreams.Int64ObservableGauge(meter)
	in.events, errs[1] = telemetry.ReactorParkedEvents.Int64ObservableGauge(meter)
	in.exhausted, errs[2] = telemetry.ReactorParkedExhausted.Int64ObservableGauge(meter)
	if in.err = errors.Join(errs[:]...); in.err != nil {
		otel.Handle(fmt.Errorf("create reactor gauges: %w", in.err))
	}
	return in
}

// report observes stats at each collection until the returned stop is
// called.
func (in instruments) report(stats func(context.Context) (parkStats, error)) (stop func()) {
	if in.err != nil {
		return func() {}
	}
	reg, err := in.meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		s, err := stats(ctx)
		if err != nil {
			return fmt.Errorf("poll reactor gauges: %w", err)
		}
		o.ObserveInt64(in.streams, int64(s.streams), in.labels)
		o.ObserveInt64(in.events, int64(s.events), in.labels)
		o.ObserveInt64(in.exhausted, int64(s.exhausted), in.labels)
		return nil
	}, in.streams, in.events, in.exhausted)
	if err != nil {
		otel.Handle(fmt.Errorf("register reactor gauges: %w", err))
		return func() {}
	}
	return func() {
		if err := reg.Unregister(); err != nil {
			otel.Handle(fmt.Errorf("unregister reactor gauges: %w", err))
		}
	}
}
