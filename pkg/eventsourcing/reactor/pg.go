package reactor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/internal/consumerhook"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// pgParks is the park list of a Reactor on a PgRuntime, in the
// parked_streams and parked_events tables of docs/postgres/schema.sql. A
// parked_streams row is the hold of one Stream; the live handler, Redrive
// and Discard lock it, so they serialize per (reactor, tenant, stream). A
// redrive keeps the lock only to claim the head and to settle it, never
// while it reacts.
//
// Its transactions never reach the handler's ctx, so a reaction's commands
// run in their own Command transactions.
type pgParks struct {
	pool    *pgxpool.Pool
	name    string
	queries pgQueries
}

func newPgParks(b consumerhook.Backend, name string) *pgParks {
	return &pgParks{pool: b.Pool, name: name, queries: newPgQueries(b.Schema)}
}

func (p *pgParks) holdIfParked(ctx context.Context, in parkInput) (bool, error) {
	var held bool
	if err := p.pool.QueryRow(ctx, p.queries.holdIfParked, p.parkArgs(in)...).Scan(&held); err != nil {
		return false, fmt.Errorf("hold event %s: %w", in.env.EventID, err)
	}
	return held, nil
}

func (p *pgParks) park(ctx context.Context, in parkInput) error {
	var next *time.Time
	if !in.nextAttempt.IsZero() {
		next = &in.nextAttempt
	}
	if _, err := p.pool.Exec(ctx, p.queries.park, append(p.parkArgs(in), next)...); err != nil {
		return fmt.Errorf("insert parked event %s: %w", in.env.EventID, err)
	}
	return nil
}

func (p *pgParks) parkArgs(in parkInput) []any {
	var cause *string
	if in.cause != "" {
		cause = &in.cause
	}
	e := in.env
	return []any{p.name, e.TenantID, e.StreamID, e.StreamType, e.StreamPosition, e.EventID, e.EventType,
		cause, in.attempts, in.at}
}

// holdInput selects the hold withHold locks.
type holdInput struct {
	stream Stream
	// auto is set to lock only a hold whose auto-redrive is due.
	auto *autoRedrive
	// now decides whether a redrive claim on the hold has expired.
	now time.Time
}

// hold is the locked state of a hold.
type hold struct {
	// redrives counts the failed auto-redrives.
	redrives int
	// claimed reports an unexpired redrive claim.
	claimed bool
}

// withHold runs fn in a transaction holding the lock on the hold of
// in.stream. It fails with ErrNotParked when there is no such hold. fn's
// transaction commits unless fn fails.
func (p *pgParks) withHold(ctx context.Context, in holdInput, fn func(tx pgx.Tx, h hold) error) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		query := p.queries.lockHold
		if in.auto != nil {
			query = p.queries.lockDueHold
		}
		var h hold
		err := tx.QueryRow(ctx, query, p.name, in.stream.Tenant, in.stream.ID, in.now).Scan(&h.redrives, &h.claimed)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotParked
		case err != nil:
			return fmt.Errorf("lock hold: %w", err)
		}
		return fn(tx, h)
	})
}

// remaining counts the held events of s and releases s when none are
// left.
func (p *pgParks) remaining(ctx context.Context, tx pgx.Tx, s Stream) (int, error) {
	var n int
	if err := tx.QueryRow(ctx, p.queries.count, p.name, s.Tenant, s.ID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count held: %w", err)
	}
	if n > 0 {
		return n, nil
	}
	if _, err := tx.Exec(ctx, p.queries.release, p.name, s.Tenant, s.ID); err != nil {
		return 0, fmt.Errorf("release hold: %w", err)
	}
	return 0, nil
}

func (p *pgParks) claimHead(ctx context.Context, in claimInput) (claimedHead, error) {
	var head claimedHead
	err := p.withHold(ctx, holdInput{stream: in.stream, auto: in.auto, now: in.now}, func(tx pgx.Tx, h hold) error {
		if h.claimed {
			return ErrRedriving
		}
		rows, err := tx.Query(ctx, p.queries.head, p.name, in.stream.Tenant, in.stream.ID)
		if err != nil {
			return fmt.Errorf("read head: %w", err)
		}
		heads, err := pgx.CollectRows(rows, scanHead)
		if err != nil {
			return fmt.Errorf("read head: %w", err)
		}
		if len(heads) == 0 {
			head.empty = true
			_, err := p.remaining(ctx, tx, in.stream)
			return err
		}
		head = heads[0]
		if _, err := tx.Exec(ctx, p.queries.claim, p.name, in.stream.Tenant, in.stream.ID, in.token, in.until); err != nil {
			return fmt.Errorf("claim head: %w", err)
		}
		return nil
	})
	return head, err
}

func (p *pgParks) settleHead(ctx context.Context, in settleInput) (int, error) {
	var remaining int
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var (
			redrives int
			ours     bool
		)
		err := tx.QueryRow(ctx, p.queries.lockClaimed, p.name, in.stream.Tenant, in.stream.ID, in.token).
			Scan(&redrives, &ours)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("lock hold: %w", err)
		case !ours:
			return fmt.Errorf("claim on %s/%s lost: %w", in.stream.Tenant, in.stream.ID, ErrRedriving)
		}
		if in.failure != nil {
			if _, err := tx.Exec(ctx, p.queries.fail, p.name, in.eventID, in.failure.Error()); err != nil {
				return fmt.Errorf("record failure: %w", err)
			}
			if err := p.reschedule(ctx, tx, rescheduleInput{stream: in.stream, auto: in.auto, redrives: redrives + 1}); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, p.queries.drop, p.name, in.eventID); err != nil {
			return fmt.Errorf("release head: %w", err)
		}
		if _, err := tx.Exec(ctx, p.queries.unclaim, p.name, in.stream.Tenant, in.stream.ID); err != nil {
			return fmt.Errorf("end claim: %w", err)
		}
		remaining, err = p.remaining(ctx, tx, in.stream)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("settle redrive of %s: %w", in.eventID, err)
	}
	return remaining, nil
}

// rescheduleInput is a hold after its redrives-th failed auto-redrive.
type rescheduleInput struct {
	stream   Stream
	auto     *autoRedrive
	redrives int
}

// reschedule moves the schedule of an auto-redriven hold on. A manual
// redrive leaves the schedule alone.
func (p *pgParks) reschedule(ctx context.Context, tx pgx.Tx, in rescheduleInput) error {
	if in.auto == nil {
		return nil
	}
	var next *time.Time
	at, exhausted := in.auto.next(in.redrives)
	if !exhausted {
		next = &at
	}
	_, err := tx.Exec(ctx, p.queries.reschedule, p.name, in.stream.Tenant, in.stream.ID, in.redrives, next, exhausted)
	if err != nil {
		return fmt.Errorf("reschedule redrive: %w", err)
	}
	return nil
}

func (p *pgParks) due(ctx context.Context, now time.Time) ([]Stream, error) {
	rows, err := p.pool.Query(ctx, p.queries.due, p.name, now)
	if err != nil {
		return nil, fmt.Errorf("query due holds: %w", err)
	}
	streams, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Stream, error) {
		var s Stream
		err := row.Scan(&s.Tenant, &s.ID)
		return s, err
	})
	if err != nil {
		return nil, fmt.Errorf("read due holds: %w", err)
	}
	return streams, nil
}

func (p *pgParks) nextDue(ctx context.Context) (time.Time, bool, error) {
	var next *time.Time
	if err := p.pool.QueryRow(ctx, p.queries.nextDue, p.name).Scan(&next); err != nil {
		return time.Time{}, false, fmt.Errorf("query next due redrive: %w", err)
	}
	if next == nil {
		return time.Time{}, false, nil
	}
	return next.UTC(), true, nil
}

func (p *pgParks) stats(ctx context.Context) (parkStats, error) {
	var st parkStats
	if err := p.pool.QueryRow(ctx, p.queries.stats, p.name).Scan(&st.streams, &st.events, &st.exhausted); err != nil {
		return parkStats{}, fmt.Errorf("count park list: %w", err)
	}
	return st, nil
}

func scanHead(row pgx.CollectableRow) (claimedHead, error) {
	var (
		h         claimedHead
		payload   eventstore.Payload
		causation *uuid.UUID
	)
	err := row.Scan(&h.env.Cursor.TransactionID, &h.env.Cursor.Position, &h.env.EventID, &h.env.TenantID,
		&h.env.StreamType, &h.env.StreamID, &h.env.StreamPosition, &payload.EventType,
		&payload.SchemaVersion, &payload.Data, &h.env.Actor, &h.env.CorrelationID, &causation,
		&h.env.RecordedAt)
	if err != nil {
		return claimedHead{}, err
	}
	h.payload = &payload
	h.env.EventType = payload.EventType
	h.env.RecordedAt = h.env.RecordedAt.UTC()
	if causation != nil {
		h.env.CausationID = *causation
	}
	return h, nil
}

func (p *pgParks) discard(ctx context.Context, in discardInput) error {
	return p.withHold(ctx, holdInput{stream: in.stream, now: in.at}, func(tx pgx.Tx, _ hold) error {
		tag, err := tx.Exec(ctx, p.queries.discard, p.name, in.stream.Tenant, in.stream.ID, in.eventID, in.at, in.by)
		if err != nil {
			return fmt.Errorf("mark discarded: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotParked
		}
		_, err = p.remaining(ctx, tx, in.stream)
		return err
	})
}

func (p *pgParks) list(ctx context.Context, tenant string) ([]Parked, error) {
	rows, err := p.pool.Query(ctx, p.queries.list, p.name, tenant)
	if err != nil {
		return nil, fmt.Errorf("query park list: %w", err)
	}
	parked, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Parked, error) {
		var (
			pk    Parked
			cause *string
			next  *time.Time
		)
		err := row.Scan(&pk.Stream.Tenant, &pk.Stream.ID, &pk.StreamType, &pk.StreamPosition, &pk.EventID,
			&pk.EventType, &cause, &pk.Attempts, &pk.ParkedAt, &next, &pk.Exhausted)
		if cause != nil {
			pk.Cause = *cause
		}
		if next != nil {
			pk.NextAttemptAt = next.UTC()
		}
		pk.ParkedAt = pk.ParkedAt.UTC()
		return pk, err
	})
	if err != nil {
		return nil, fmt.Errorf("read park list: %w", err)
	}
	return parked, nil
}

// pgQueries are the park list statements for one schema. $1 is always the
// Reactor's name.
type pgQueries struct {
	holdIfParked, park                         string
	lockHold, head, count, release, fail, drop string
	claim, lockClaimed, unclaim                string
	lockDueHold, reschedule, due, nextDue      string
	discard, list, stats                       string
}

func newPgQueries(schema string) pgQueries {
	streams := pgx.Identifier{schema, "parked_streams"}.Sanitize()
	parked := pgx.Identifier{schema, "parked_events"}.Sanitize()
	events := pgx.Identifier{schema, "events"}.Sanitize()
	insert := `INSERT INTO ` + parked + ` (consumer, tenant_id, stream_id, stream_type, stream_position,
			event_id, event_type, cause, attempts, parked_at)`
	stream := `consumer = $1 AND tenant_id = $2 AND stream_id = $3`
	// claimed binds the redrive clock to $4.
	claimed := `redrive_claim IS NOT NULL AND redrive_claimed_until > $4`

	return pgQueries{
		holdIfParked: `WITH hold AS (
				SELECT consumer FROM ` + streams + ` WHERE ` + stream + ` FOR UPDATE
			), held AS (
				` + insert + `
				SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10 FROM hold
				ON CONFLICT (consumer, event_id) DO NOTHING
			)
			SELECT EXISTS (SELECT 1 FROM hold)`,
		park: `WITH hold AS (
				INSERT INTO ` + streams + ` (consumer, tenant_id, stream_id, parked_at, next_attempt_at)
				VALUES ($1, $2, $3, $10, $11) ON CONFLICT DO NOTHING
			)
			` + insert + ` VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (consumer, event_id) DO NOTHING`,

		lockHold: `SELECT redrives, ` + claimed + ` FROM ` + streams + ` WHERE ` + stream + ` FOR UPDATE`,
		lockDueHold: `SELECT redrives, ` + claimed + ` FROM ` + streams + ` WHERE ` + stream + `
			AND NOT exhausted AND next_attempt_at <= $4 FOR UPDATE`,
		claim: `UPDATE ` + streams + ` SET redrive_claim = $4, redrive_claimed_until = $5
			WHERE ` + stream,
		lockClaimed: `SELECT redrives, redrive_claim IS NOT DISTINCT FROM $4 FROM ` + streams + `
			WHERE ` + stream + ` FOR UPDATE`,
		unclaim: `UPDATE ` + streams + ` SET redrive_claim = NULL, redrive_claimed_until = NULL
			WHERE ` + stream,
		reschedule: `UPDATE ` + streams + ` SET redrives = $4, next_attempt_at = $5, exhausted = $6
			WHERE ` + stream,
		due: `SELECT tenant_id, stream_id FROM ` + streams + `
			WHERE consumer = $1 AND NOT exhausted AND next_attempt_at <= $2
			ORDER BY next_attempt_at, tenant_id, stream_id`,
		nextDue: `SELECT min(next_attempt_at) FROM ` + streams + ` WHERE consumer = $1 AND NOT exhausted`,
		stats: `SELECT
				(SELECT count(*)::int FROM ` + streams + ` WHERE consumer = $1),
				(SELECT count(*)::int FROM ` + parked + ` WHERE consumer = $1 AND discarded_at IS NULL),
				(SELECT count(*)::int FROM ` + streams + ` WHERE consumer = $1 AND exhausted)`,
		head: `SELECT e.transaction_id::text::numeric, e.global_position, e.event_id, e.tenant_id,
				e.stream_type, e.stream_id, e.stream_position, e.event_type, e.schema_version, e.event_data,
				e.actor, e.correlation_id, e.causation_id, e.recorded_at
			FROM ` + parked + ` p JOIN ` + events + ` e ON e.event_id = p.event_id
			WHERE p.consumer = $1 AND p.tenant_id = $2 AND p.stream_id = $3 AND p.discarded_at IS NULL
			ORDER BY p.stream_position LIMIT 1`,
		count:   `SELECT count(*)::int FROM ` + parked + ` WHERE ` + stream + ` AND discarded_at IS NULL`,
		release: `DELETE FROM ` + streams + ` WHERE ` + stream,
		fail: `UPDATE ` + parked + ` SET cause = $3, attempts = attempts + 1
			WHERE consumer = $1 AND event_id = $2 AND discarded_at IS NULL`,
		drop: `DELETE FROM ` + parked + ` WHERE consumer = $1 AND event_id = $2 AND discarded_at IS NULL`,

		discard: `UPDATE ` + parked + ` SET discarded_at = $5, discarded_by = $6
			WHERE ` + stream + ` AND event_id = $4 AND discarded_at IS NULL`,
		list: `SELECT p.tenant_id, p.stream_id, p.stream_type, p.stream_position, p.event_id, p.event_type,
				p.cause, p.attempts, p.parked_at, s.next_attempt_at, s.exhausted
			FROM ` + parked + ` p JOIN ` + streams + ` s USING (consumer, tenant_id, stream_id)
			WHERE p.consumer = $1 AND ($2 = '' OR p.tenant_id = $2) AND p.discarded_at IS NULL
			ORDER BY s.parked_at, p.tenant_id, p.stream_id, p.stream_position`,
	}
}
