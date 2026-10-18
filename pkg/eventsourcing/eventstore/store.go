package eventstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/safeid"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
)

// Store is the event log a Repository reads and appends to. It is
// implemented by InMemoryStore and the Postgres store; both go through a
// Codec.
type Store interface {
	// load returns the events of a Stream in stream order, decoded. Inside
	// the Command transaction for that Stream it includes the events
	// appended so far.
	load(ctx context.Context, in loadInput) ([]Envelope, error)
	// append writes events after expected in the Stream held by the Command
	// transaction in ctx and returns the Cursor of the last one. It fails
	// with ErrConcurrencyConflict when the Stream is not at expected.
	append(ctx context.Context, in appendInput) (Cursor, error)
	// findCommand returns the command recorded under id for the Tenant of
	// tx, including one recorded earlier in tx.
	findCommand(ctx context.Context, tx *commandTx, id uuid.UUID) (processedCommand, bool, error)
	// recordCommand records cmd in tx. It fails with ErrConcurrencyConflict
	// when another transaction recorded cmd.id for the Tenant meanwhile.
	recordCommand(ctx context.Context, tx *commandTx, cmd processedCommand) error
}

// processedCommand is a command that ran under a Command ID: the Stream it
// changed and the result it returned.
type processedCommand struct {
	id     uuid.UUID
	stream uuid.UUID
	result CommandResult
}

type loadInput struct {
	tenant string
	stream uuid.UUID
	// upTo limits the result to stream positions <= upTo; 0 loads all.
	upTo int
}

type appendInput struct {
	tx         *commandTx
	streamType string
	expected   int
	events     []aggregate.Event
	// traceContext is the W3C traceparent of the send span, stored with
	// every event; "" stores none.
	traceContext string
}

// StoreOption configures a Store.
type StoreOption func(*storeOptions)

type storeOptions struct {
	clock func() time.Time
}

// WithClock sets the clock that stamps recorded_at on appended events and
// processed commands. The default is time.Now().UTC().
func WithClock(clock func() time.Time) StoreOption {
	return func(o *storeOptions) { o.clock = clock }
}

func newStoreOptions(opts []StoreOption) storeOptions {
	o := storeOptions{clock: func() time.Time { return time.Now().UTC() }}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

const systemActorPrefix = "system:"

// metadata is what an append records about who and what caused its events
// (ADR-0010).
type metadata struct {
	actor       string
	correlation string
	causation   uuid.UUID
}

func resolveMetadata(ctx context.Context, tx *commandTx) (metadata, error) {
	actor, err := resolveActor(ctx)
	if err != nil {
		return metadata{}, err
	}
	causation, _ := ctxutil.CausationID(ctx)
	return metadata{actor: actor, correlation: resolveCorrelation(ctx, tx), causation: causation}, nil
}

// resolveActor prefers ctxutil.Actor, then Claims.UserID. A user ID with the
// reserved "system:" prefix is rejected, so users cannot pose as library
// components.
func resolveActor(ctx context.Context) (string, error) {
	if actor, ok := ctxutil.Actor(ctx); ok {
		return actor, nil
	}
	user, _ := ctxutil.UserID(ctx)
	switch {
	case user == "":
		return "", ErrNoActor
	case strings.HasPrefix(user, systemActorPrefix):
		return "", fmt.Errorf("user %q uses the reserved %q prefix: %w", user, systemActorPrefix, ErrNoActor)
	}
	return user, nil
}

func resolveCorrelation(ctx context.Context, tx *commandTx) string {
	if id, ok := ctxutil.CorrelationID(ctx); ok {
		return id
	}
	if id, _ := ctxutil.RequestID(ctx); safeid.Valid(id) {
		return id
	}
	return tx.correlation
}
