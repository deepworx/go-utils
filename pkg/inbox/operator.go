package inbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/deepworx/go-utils/pkg/ctxutil"
)

// DefaultListLimit is the number of messages List returns when
// Filter.Limit is not positive.
const DefaultListLimit = 100

// Filter selects the messages List returns.
type Filter struct {
	// Source restricts the list to one Source; empty means all Sources.
	Source string
	// Status restricts the list to one Status; empty means all.
	Status Status
	// Limit caps the number of messages. Not positive uses
	// DefaultListLimit.
	Limit int
}

// Record is a stored message with its processing state.
type Record struct {
	Message
	// ID is the message's row ID.
	ID     int64
	Status Status
	// Attempts counts the claims since insert or the last Redrive.
	Attempts int
	// Error is the last failure, the Ignore reason or the Discard reason.
	Error         string
	ReceivedAt    time.Time
	NextAttemptAt time.Time
	// SettledAt is when the message reached a final status; zero while
	// pending or Parked.
	SettledAt   time.Time
	DiscardedAt time.Time
	DiscardedBy string
}

// List returns the messages matching f, oldest first, with their Payload.
// It is an Operator call: it lists every Tenant's messages (ADR-0014).
func (s *Store) List(ctx context.Context, f Filter) ([]Record, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, source, key, type, COALESCE(tenant_id, ''), payload, status, attempts,
		       COALESCE(error, ''), received_at, next_attempt_at, settled_at,
		       discarded_at, COALESCE(discarded_by, '')
		FROM `+s.table+`
		WHERE ($1 = '' OR source = $1) AND ($2 = '' OR status = $2)
		ORDER BY id
		LIMIT $3`,
		f.Source, string(f.Status), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("inbox: list: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanRecord)
	if err != nil {
		return nil, fmt.Errorf("inbox: list: %w", err)
	}
	return out, nil
}

func scanRecord(row pgx.CollectableRow) (Record, error) {
	var (
		r                    Record
		settledAt, discarded *time.Time
	)
	err := row.Scan(&r.ID, &r.Source, &r.Key, &r.Type, &r.Tenant, &r.Payload, &r.Status, &r.Attempts,
		&r.Error, &r.ReceivedAt, &r.NextAttemptAt, &settledAt, &discarded, &r.DiscardedBy)
	if settledAt != nil {
		r.SettledAt = *settledAt
	}
	if discarded != nil {
		r.DiscardedAt = *discarded
	}
	return r, err
}

// Redrive makes the Parked message id due now with its attempts reset, so
// its next attempt counts as the first. It wakes the Workers listening for
// the message's Source. It fails with ErrNotFound for an unknown id and
// ErrNotParked for a message that is not Parked. It is an Operator call:
// id is not checked against the ctx Tenant (ADR-0014).
func (s *Store) Redrive(ctx context.Context, id int64) error {
	var released bool
	err := s.pool.QueryRow(ctx, `
		WITH released AS (
			UPDATE `+s.table+`
			SET status = 'pending',
			    attempts = 0,
			    claim_generation = claim_generation + 1,
			    claimed_until = NULL,
			    next_attempt_at = $2,
			    settled_at = NULL,
			    error = NULL
			WHERE id = $1 AND status = 'parked'
			RETURNING source
		)
		SELECT pg_notify($3, source) IS NOT NULL FROM released`,
		id, s.now(), s.channel,
	).Scan(&released)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.wrongStatus(ctx, id, ErrNotParked)
	}
	if err != nil {
		return fmt.Errorf("inbox: redrive message %d: %w", id, err)
	}
	return nil
}

// Discard settles the pending or Parked message id as discarded. The row is
// kept with the discard time and the ctxutil.ResolveActor of ctx, so its key
// stays deduplicated and the decision stays traceable. A non-empty reason
// replaces the message's Error. It fails with ctxutil.ErrNoActor when ctx
// names no one, ErrNotFound for an unknown id and ErrSettled for a message
// that is already done, ignored or discarded. It is an Operator call: id is
// not checked against the ctx Tenant (ADR-0014).
func (s *Store) Discard(ctx context.Context, id int64, reason string) error {
	by, err := ctxutil.ResolveActor(ctx)
	if err != nil {
		return fmt.Errorf("inbox: discard message %d: %w", id, err)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE `+s.table+`
		SET status = 'discarded',
		    claimed_until = NULL,
		    settled_at = $2,
		    discarded_at = $2,
		    discarded_by = $3,
		    error = COALESCE(NULLIF($4, ''), error)
		WHERE id = $1 AND status IN ('pending', 'parked')`,
		id, s.now(), by, reason,
	)
	if err != nil {
		return fmt.Errorf("inbox: discard message %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return s.wrongStatus(ctx, id, ErrSettled)
	}
	return nil
}

// Cleanup deletes the done, ignored and discarded messages settled before
// before and reports how many it deleted. Pending and Parked messages are
// never deleted. A deleted key can be inserted again.
func (s *Store) Cleanup(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM `+s.table+`
		WHERE status IN ('done', 'ignored', 'discarded') AND settled_at < $1`,
		before,
	)
	if err != nil {
		return 0, fmt.Errorf("inbox: cleanup before %s: %w", before.Format(time.RFC3339), err)
	}
	return tag.RowsAffected(), nil
}

// wrongStatus explains why an operator call matched no row: the message
// does not exist, or it has a status the call does not accept.
func (s *Store) wrongStatus(ctx context.Context, id int64, wrong error) error {
	var status Status
	err := s.pool.QueryRow(ctx, `SELECT status FROM `+s.table+` WHERE id = $1`, id).Scan(&status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %d", ErrNotFound, id)
	case err != nil:
		return fmt.Errorf("inbox: read message %d: %w", id, err)
	default:
		return fmt.Errorf("%w: message %d is %s", wrong, id, status)
	}
}
