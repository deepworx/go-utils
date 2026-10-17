package reactor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

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
}

// replayFunc reacts once to a held event.
type replayFunc func(ctx context.Context, env eventstore.Envelope) error

type redriveInput struct {
	stream Stream
	replay replayFunc
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
	// redriveHead replays the first held event of a Stream and releases it
	// on success. It fails with ErrNotParked when the Stream is not Parked
	// and releases a Stream whose hold is empty.
	redriveHead(ctx context.Context, in redriveInput) (headResult, error)
	discard(ctx context.Context, in discardInput) error
	list(ctx context.Context, tenant string) ([]Parked, error)
}

func newParkStore(b consumerhook.Backend, name string) parkStore {
	if b.Pool != nil {
		return newPgParks(b, name)
	}
	return newMemParks(b.Codec)
}

// Redrive replays the events held for s in order, one attempt each, and
// releases each that succeeds. It stops at the first failure, which it
// returns together with how many events remain held. It goes on until the
// hold is empty, including events held while it runs, and never moves the
// Cursor. It fails with ErrNotParked when s is not Parked.
func (r *Reactor) Redrive(ctx context.Context, s Stream) (RedriveResult, error) {
	var res RedriveResult
	for {
		head, err := r.parks.redriveHead(ctx, redriveInput{stream: s, replay: r.react})
		switch {
		case errors.Is(err, ErrNotParked) && res.Replayed > 0:
			return res, nil
		case err != nil:
			return res, fmt.Errorf("redrive %s/%s: %w", s.Tenant, s.ID, err)
		}
		res.Remaining = head.remaining
		if head.failure != nil {
			return res, fmt.Errorf("redrive %s/%s: %w", s.Tenant, s.ID, head.failure)
		}
		if head.replayed {
			res.Replayed++
		}
		if head.remaining == 0 {
			if res.Replayed == 0 {
				return res, fmt.Errorf("redrive %s/%s: %w", s.Tenant, s.ID, ErrNotParked)
			}
			return res, nil
		}
	}
}

// Discard releases one held event of s without reacting to it. The event
// stays recorded as discarded, by the Actor or user in ctx. When it was the
// last held event, s is no longer Parked. It fails with ErrNotParked when
// s does not hold the event, and with eventstore.ErrNoActor when ctx names
// no one.
func (r *Reactor) Discard(ctx context.Context, s Stream, eventID uuid.UUID) error {
	by, ok := ctxutil.Actor(ctx)
	if !ok {
		by, _ = ctxutil.UserID(ctx)
	}
	if by == "" {
		return fmt.Errorf("discard %s: %w", eventID, eventstore.ErrNoActor)
	}
	err := r.parks.discard(ctx, discardInput{stream: s, eventID: eventID, by: by, at: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("discard %s of %s/%s: %w", eventID, s.Tenant, s.ID, err)
	}
	return nil
}

// Parked lists the held events of tenant, or of every Tenant for "". The
// events of one Stream are in Stream order, Streams in the order they were
// Parked.
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
