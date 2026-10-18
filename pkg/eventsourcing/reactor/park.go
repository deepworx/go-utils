package reactor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/backoff"
	"github.com/deepworx/go-utils/internal/consumerhook"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// Stream names a Stream of one Tenant.
type Stream struct {
	Tenant string
	ID     uuid.UUID
}

// Parked is one event a Reactor holds.
type Parked struct {
	Stream         Stream
	StreamType     string
	StreamPosition int
	EventID        uuid.UUID
	EventType      string
	// Cause is the last failure; empty for an event held behind the one
	// that failed.
	Cause string
	// Attempts counts the failed reactions, redrives included.
	Attempts int
	ParkedAt time.Time
	// NextAttemptAt is when the Stream is redriven automatically next; zero
	// when auto-redrive is off or Exhausted.
	NextAttemptAt time.Time
	// Exhausted reports a Stream whose auto-redrive schedule is spent; only
	// a manual Redrive or Discard releases it.
	Exhausted bool
}

// RedriveResult is how far a Redrive got.
type RedriveResult struct {
	// Replayed counts the events that reacted and were released.
	Replayed int
	// Remaining counts the events still held.
	Remaining int
}

// parkInput is one event to park or hold.
type parkInput struct {
	env eventstore.Envelope
	// payload is set for an undecodable event; env.Event is nil then.
	payload  *eventstore.Payload
	cause    string
	attempts int
	at       time.Time
	// nextAttempt is the first auto-redrive of a new hold; zero for none.
	nextAttempt time.Time
}

// replayFunc reacts once to a held event.
type replayFunc func(ctx context.Context, env eventstore.Envelope) error

type redriveInput struct {
	stream Stream
	replay replayFunc
	// auto is set for an auto-redrive: only a due hold is replayed, and a
	// failure moves its schedule on.
	auto *autoRedrive
}

// autoRedrive is an auto-redrive at now on schedule steps.
type autoRedrive struct {
	now   time.Time
	steps backoff.Steps
}

// next returns the schedule of a hold after its redrives-th failed
// auto-redrive: the next attempt, or exhausted.
func (a autoRedrive) next(redrives int) (next time.Time, exhausted bool) {
	d, ok := a.steps.Delay(redrives+1, nil)
	if !ok {
		return time.Time{}, true
	}
	return a.now.Add(d), false
}

// claimInput claims the head of the hold of stream for one redrive.
type claimInput struct {
	stream Stream
	// auto is set for an auto-redrive: only a due hold is claimed.
	auto  *autoRedrive
	token uuid.UUID
	now   time.Time
	// until is when the claim expires, so a crashed redrive does not block
	// the hold for ever.
	until time.Time
}

// claimedHead is the head of a hold claimed for a redrive.
type claimedHead struct {
	env eventstore.Envelope
	// payload is set when env.Event still needs decoding.
	payload *eventstore.Payload
	// empty reports a hold without events, which the claim released.
	empty bool
}

// settleInput records the outcome of replaying a claimed head and ends the
// claim.
type settleInput struct {
	stream  Stream
	token   uuid.UUID
	eventID uuid.UUID
	// failure is the replay error; nil releases the head.
	failure error
	auto    *autoRedrive
}

// headResult is the outcome of replaying the head of a hold.
type headResult struct {
	replayed  bool
	remaining int
	failure   error
}

type discardInput struct {
	stream  Stream
	eventID uuid.UUID
	by      string
	at      time.Time
}

// parkStore keeps the park list. Each Stream's hold is serialized: the live
// handler, Redrive and Discard never interleave on one Stream.
type parkStore interface {
	// holdIfParked adds in to the hold of its Stream when the Stream is
	// Parked, and reports whether it was.
	holdIfParked(ctx context.Context, in parkInput) (bool, error)
	// park Parks the Stream of in with in as its first held event.
	park(ctx context.Context, in parkInput) error
	// claimHead claims the first held event of a Stream for a redrive,
	// under the hold's lock but without keeping it. It fails with
	// ErrNotParked when the Stream is not Parked (or not due) and with
	// ErrRedriving while another unexpired claim exists, and releases a
	// Stream whose hold is empty.
	claimHead(ctx context.Context, in claimInput) (claimedHead, error)
	// settleHead releases the claimed head on success, or records the
	// failure and moves an auto-redrive's schedule on. It ends the claim
	// and returns how many events remain held. It fails with ErrRedriving
	// when the claim was lost.
	settleHead(ctx context.Context, in settleInput) (remaining int, err error)
	// discard releases one held event without reacting to it. It fails
	// with ErrRedriving while an unexpired claim exists, whichever event is
	// named, and otherwise with ErrNotParked when the Stream does not hold
	// the event.
	discard(ctx context.Context, in discardInput) error
	list(ctx context.Context, tenant string) ([]Parked, error)
	// due lists the Streams whose auto-redrive is due at now, earliest
	// first.
	due(ctx context.Context, now time.Time) ([]Stream, error)
	// nextDue returns the earliest scheduled auto-redrive, or false when
	// none is scheduled.
	nextDue(ctx context.Context) (time.Time, bool, error)
	stats(ctx context.Context) (parkStats, error)
}

// parkStats counts the park list.
type parkStats struct {
	streams, events, exhausted int
}

func newParkStore(b consumerhook.Backend, name string) parkStore {
	if b.Pool != nil {
		return newPgParks(b, name)
	}
	return newMemParks()
}

const (
	// claimMargin is added to the reaction timeout for a redrive claim's
	// lifetime.
	claimMargin = 30 * time.Second
	// settleTimeout bounds recording a redrive's outcome, which runs even
	// after the caller's ctx ended because the reaction already ran.
	settleTimeout = 5 * time.Second
)

// redriveHead claims the head of the hold of in.stream, replays it outside
// any transaction or lock and settles the outcome, so the live handler and
// Discard never wait for a reaction.
func (r *Reactor) redriveHead(ctx context.Context, in redriveInput) (headResult, error) {
	token, err := uuid.NewV7()
	if err != nil {
		return headResult{}, fmt.Errorf("generate redrive claim: %w", err)
	}
	now := r.now()
	head, err := r.parks.claimHead(ctx, claimInput{
		stream: in.stream, auto: in.auto, token: token, now: now, until: now.Add(r.opts.timeout + claimMargin),
	})
	if err != nil || head.empty {
		return headResult{}, err
	}
	failure := r.replayHead(ctx, in.replay, head)

	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	remaining, err := r.parks.settleHead(settleCtx, settleInput{
		stream: in.stream, token: token, eventID: head.env.EventID, failure: failure, auto: in.auto,
	})
	if err != nil {
		return headResult{}, err
	}
	return headResult{replayed: failure == nil, remaining: remaining, failure: failure}, nil
}

func (r *Reactor) replayHead(ctx context.Context, replay replayFunc, h claimedHead) error {
	env := h.env
	if h.payload != nil {
		event, err := r.codec.Decode(*h.payload)
		if err != nil {
			return fmt.Errorf("event %s: %w", env.EventID, err)
		}
		env.Event = event
	}
	return replay(ctx, env)
}

// Redrive replays the events held for s in order, one attempt each, and
// releases each that succeeds. It stops at the first failure, which it
// returns together with how many events remain held. It goes on until the
// hold is empty, including events held while it runs, and never moves the
// Cursor. It fails with ErrNotParked when s is not Parked. It is an
// Operator call: s.Tenant is not checked against the ctx Tenant (ADR-0014).
func (r *Reactor) Redrive(ctx context.Context, s Stream) (RedriveResult, error) {
	res, failure, err := r.redrive(ctx, redriveInput{stream: s, replay: r.react})
	if err == nil {
		err = failure
	}
	if err != nil {
		return res, fmt.Errorf("redrive %s/%s: %w", s.Tenant, s.ID, err)
	}
	return res, nil
}

// redrive replays the hold of in.stream head by head until it is empty or
// a replay fails with failure. err is ErrNotParked when nothing was held,
// or not due for an auto-redrive.
func (r *Reactor) redrive(ctx context.Context, in redriveInput) (res RedriveResult, failure, err error) {
	for {
		head, err := r.redriveHead(ctx, in)
		switch {
		case errors.Is(err, ErrNotParked) && res.Replayed > 0:
			return res, nil, nil
		case err != nil:
			return res, nil, err
		}
		res.Remaining = head.remaining
		if head.failure != nil {
			return res, head.failure, nil
		}
		if head.replayed {
			res.Replayed++
		}
		if head.remaining == 0 {
			if res.Replayed == 0 {
				return res, nil, ErrNotParked
			}
			return res, nil, nil
		}
	}
}

// Discard releases one held event of s without reacting to it. The event
// stays recorded as discarded, by ctxutil.ResolveActor. When it was the
// last held event, s is no longer Parked. It fails with ErrNotParked when
// s does not hold the event, with ErrRedriving while a Redrive or
// auto-redrive of s is in progress (whichever event is named), and with
// eventstore.ErrNoActor when ctx names no one. It is an Operator call:
// s.Tenant is not checked against the ctx Tenant (ADR-0014).
func (r *Reactor) Discard(ctx context.Context, s Stream, eventID uuid.UUID) error {
	by, err := ctxutil.ResolveActor(ctx)
	if err != nil {
		return fmt.Errorf("discard %s: %w", eventID, err)
	}
	if err := r.parks.discard(ctx, discardInput{stream: s, eventID: eventID, by: by, at: r.now()}); err != nil {
		return fmt.Errorf("discard %s of %s/%s: %w", eventID, s.Tenant, s.ID, err)
	}
	return nil
}

// Parked lists the held events of tenant, or of every Tenant for "". The
// events of one Stream are in Stream order, Streams in the order they were
// Parked. It is an Operator call: tenant is not checked against the ctx
// Tenant, and Parked.Cause carries raw error text (ADR-0014).
func (r *Reactor) Parked(ctx context.Context, tenant string) ([]Parked, error) {
	p, err := r.parks.list(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("list parked of %q: %w", tenant, err)
	}
	return p, nil
}

func newParked(in parkInput) Parked {
	return Parked{
		Stream:         Stream{Tenant: in.env.TenantID, ID: in.env.StreamID},
		StreamType:     in.env.StreamType,
		StreamPosition: in.env.StreamPosition,
		EventID:        in.env.EventID,
		EventType:      in.env.EventType,
		Cause:          in.cause,
		Attempts:       in.attempts,
		ParkedAt:       in.at,
	}
}
