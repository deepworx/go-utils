package consumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/internal/backoff"
	"github.com/deepworx/go-utils/internal/telemetry"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// Run processes events until ctx ends, then returns nil. A Stopped Consumer
// keeps running and waits for Runtime.Resume. Run fails with ErrRunning when
// the Consumer already runs.
func (c *Consumer) Run(ctx context.Context) error {
	if !c.running.CompareAndSwap(false, true) {
		return fmt.Errorf("run %q: %w", c.name, ErrRunning)
	}
	defer c.running.Store(false)

	for {
		leaseCtx, release, err := c.port.acquire(ctx)
		switch {
		case err == nil:
			c.runHeld(leaseCtx)
			release()
		case errors.Is(err, ErrRunning):
		default:
			c.logger.ErrorContext(ctx, "acquire consumer lease", telemetry.LogError, err)
		}
		if !c.wait(ctx, nil) {
			return nil
		}
	}
}

// runHeld processes events while ctx, the lease ctx, lasts.
func (c *Consumer) runHeld(ctx context.Context) {
	for {
		changed := c.port.changed()
		progressed, err := c.step(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, ErrStopped) {
			c.logger.ErrorContext(ctx, "process consumer batch", telemetry.LogError, err)
		}
		if progressed && err == nil {
			continue
		}
		if !c.wait(ctx, changed) {
			return
		}
	}
}

// wait blocks until new events may be visible, the Consumer was resumed, the
// poll interval passed or ctx ended. It reports false when ctx ended.
func (c *Consumer) wait(ctx context.Context, changed <-chan struct{}) bool {
	timer := time.NewTimer(backoff.Poll(c.opts.pollInterval, nil))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-changed:
	case <-c.port.resumed():
	case <-timer.C:
	}
	return true
}

// step processes one batch after the Cursor. progressed reports whether
// there was one.
func (c *Consumer) step(ctx context.Context) (progressed bool, err error) {
	state, err := c.port.load(ctx)
	if err != nil {
		return false, fmt.Errorf("load cursor: %w", err)
	}
	if state.stopped != nil {
		return false, c.stoppedError(state.stopped)
	}
	records, err := c.port.read(ctx, readInput{after: state.cursor, limit: c.opts.batchSize})
	if err != nil {
		return false, fmt.Errorf("read events: %w", err)
	}
	if len(records) == 0 {
		return false, nil
	}
	return true, c.process(ctx, state.cursor, records)
}

// Drain processes events synchronously up to the head of the log at the
// time of the call. It fails with ErrRunning while the Consumer runs or
// drains, and with a wrapped ErrStopped when the Consumer is or goes
// Stopped.
func (c *Consumer) Drain(ctx context.Context) error {
	ctx, release, err := c.port.acquire(ctx)
	if err != nil {
		return fmt.Errorf("drain %q: %w", c.name, err)
	}
	defer release()

	head, err := c.port.head(ctx)
	if err != nil {
		return fmt.Errorf("drain %q: head: %w", c.name, err)
	}
	for {
		state, err := c.port.load(ctx)
		if err != nil {
			return fmt.Errorf("drain %q: load cursor: %w", c.name, err)
		}
		if state.stopped != nil {
			return c.stoppedError(state.stopped)
		}
		if state.cursor.Compare(head) >= 0 {
			return nil
		}
		records, err := c.port.read(ctx, readInput{after: state.cursor, limit: c.opts.batchSize})
		if err != nil {
			return fmt.Errorf("drain %q: read events: %w", c.name, err)
		}
		records = upTo(records, head)
		if len(records) == 0 {
			return nil
		}
		if err := c.process(ctx, state.cursor, records); err != nil {
			return err
		}
	}
}

func upTo(records []record, head eventstore.Cursor) []record {
	for i, r := range records {
		if r.env.Cursor.Compare(head) > 0 {
			return records[:i]
		}
	}
	return records
}

// Status returns the Consumer's Cursor, its pending events and its Stopped
// state.
func (c *Consumer) Status(ctx context.Context) (Status, error) {
	s, err := c.port.status(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("status %q: %w", c.name, err)
	}
	return s, nil
}

func (c *Consumer) stoppedError(s *Stop) error {
	return fmt.Errorf("consumer %q since %s: %w: %s", c.name, s.At.Format(time.RFC3339), ErrStopped, s.Cause)
}

// process handles records, which follow the Cursor from, and advances the
// Cursor past every event it handled.
func (c *Consumer) process(ctx context.Context, from eventstore.Cursor, records []record) error {
	ctx, span := c.tracer.Start(ctx, telemetry.SpanProcess(c.name),
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			telemetry.AttrConsumer.String(c.name),
			telemetry.AttrBatchSize.Int(len(records)),
		),
	)
	defer span.End()

	var err error
	if c.mode == Transactional {
		err = c.processTransactional(ctx, from, records)
	} else {
		err = c.processOwn(ctx, from, records)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// processTransactional commits records in one Cursor transaction. When an
// event fails, the events before it commit on their own and the failing one
// is retried alone; the failed batch counts as its first attempt.
func (c *Consumer) processTransactional(ctx context.Context, from eventstore.Cursor, records []record) error {
	failedAt, err := c.commit(ctx, from, records)
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return context.Cause(ctx)
	case errors.Is(err, errFenced):
		return err
	}
	if failedAt > 0 {
		if err := c.processTransactional(ctx, from, records[:failedAt]); err != nil {
			return err
		}
		from = records[failedAt-1].env.Cursor
	}
	failed := records[failedAt : failedAt+1]
	return c.retryEvent(ctx, failed[0], err, func() error {
		_, err := c.commit(ctx, from, failed)
		return err
	})
}

// commit runs records and the Cursor update in one Cursor transaction.
// failedAt is the index of the failing event, or 0 when the transaction
// itself failed.
func (c *Consumer) commit(ctx context.Context, from eventstore.Cursor, records []record) (failedAt int, err error) {
	last := records[len(records)-1].env.Cursor
	err = c.port.transact(ctx, func(ctx context.Context) error {
		for i, r := range records {
			if err := c.handle(ctx, r); err != nil {
				failedAt = i
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		return c.port.advance(ctx, move{from: from, to: last})
	})
	return failedAt, err
}

// processOwn handles each record and then advances the Cursor past it.
func (c *Consumer) processOwn(ctx context.Context, from eventstore.Cursor, records []record) error {
	for _, r := range records {
		if err := c.handle(ctx, r); err != nil {
			if err := c.retryEvent(ctx, r, err, func() error { return c.handle(ctx, r) }); err != nil {
				return err
			}
		}
		if err := c.port.advance(ctx, move{from: from, to: r.env.Cursor}); err != nil {
			return fmt.Errorf("advance cursor: %w", err)
		}
		from = r.env.Cursor
	}
	return nil
}

// retryEvent retries r after its first failure err until try succeeds, ctx
// ends or the failure stops the Consumer.
func (c *Consumer) retryEvent(ctx context.Context, r record, err error, try func() error) error {
	for failed := 1; ; failed++ {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		delay, ok := c.retry.Delay(failed, nil)
		if !ok || isPermanent(err) {
			return c.stop(ctx, r, err)
		}
		c.logger.WarnContext(ctx, "consumer event failed, retrying",
			append(eventAttrs(r.env), telemetry.LogAttempt, failed, telemetry.LogError, err)...)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		case <-timer.C:
		}
		if err = try(); err == nil {
			return nil
		}
	}
}

func (c *Consumer) stop(ctx context.Context, r record, cause error) error {
	if err := c.port.stop(ctx, Stop{At: time.Now(), Cause: cause.Error()}); err != nil {
		return fmt.Errorf("stop after %w: %w", cause, err)
	}
	c.logger.ErrorContext(ctx, "consumer stopped", append(eventAttrs(r.env), telemetry.LogError, cause)...)
	return fmt.Errorf("consumer %q at event %s: %w: %w", c.name, r.env.EventID, ErrStopped, cause)
}

func eventAttrs(env eventstore.Envelope) []any {
	return []any{
		telemetry.LogTenantID, env.TenantID,
		telemetry.LogStreamType, env.StreamType,
		telemetry.LogStreamID, env.StreamID,
		telemetry.LogEventID, env.EventID,
		telemetry.LogEventType, env.EventType,
	}
}

// handle decodes r and runs the handler with the event's Tenant and
// Correlation in ctx. A decode error or a panic is Permanent.
func (c *Consumer) handle(ctx context.Context, r record) (err error) {
	ctx = ctxutil.WithTenantID(ctx, r.env.TenantID)
	ctx = ctxutil.WithCorrelationID(ctx, r.env.CorrelationID)
	event, err := c.port.codec().Decode(r.payload)
	if err != nil {
		err = Permanent(fmt.Errorf("event %s: %w", r.env.EventID, err))
		if c.opts.decodeFailed != nil {
			return c.opts.decodeFailed(ctx, r.env, r.payload, err)
		}
		return err
	}
	env := r.env
	env.Event = event

	if c.opts.eventSpans {
		var span trace.Span
		ctx, span = c.tracer.Start(ctx, telemetry.SpanHandle(env.EventType), trace.WithAttributes(
			telemetry.AttrConsumer.String(c.name),
			telemetry.AttrTenantID.String(env.TenantID),
			telemetry.AttrStreamID.String(env.StreamID.String()),
			telemetry.AttrEventID.String(env.EventID.String()),
			telemetry.AttrEventType.String(env.EventType),
			telemetry.AttrPosition.Int64(env.Cursor.Position),
		))
		defer func() {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			span.End()
		}()
	}
	defer func() {
		if p := recover(); p != nil {
			err = Permanent(fmt.Errorf("handler panic: %v", p))
		}
	}()
	return c.handler.Handle(ctx, env)
}
