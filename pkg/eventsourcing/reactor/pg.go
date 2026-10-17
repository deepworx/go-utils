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
// and Discard lock it, so they serialize per (reactor, tenant, stream).
//
// Its transactions never reach the handler's ctx, so a reaction's commands
// run in their own Command transactions.
type pgParks struct {
	pool    *pgxpool.Pool
	codec   *eventstore.Codec
	name    string
	queries pgQueries
}

func newPgParks(b consumerhook.Backend, name string) *pgParks {
	return &pgParks{pool: b.Pool, codec: b.Codec, name: name, queries: newPgQueries(b.Schema)}
}

func (p *pgParks) holdIfParked(ctx context.Context, in parkInput) (bool, error) {
	var held bool
	err := p.pool.QueryRow(ctx, p.queries.holdIfParked, p.parkArgs(in)...).Scan(&held)
	return held, err
}

func (p *pgParks) park(ctx context.Context, in parkInput) error {
	var next *time.Time
	if !in.nextAttempt.IsZero() {
		next = &in.nextAttempt
	}
	_, err := p.pool.Exec(ctx, p.queries.park, append(p.parkArgs(in), next)...)
	return err
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

// withHold runs fn in a transaction holding the lock on the hold of s,
// passing the hold's failed auto-redrives. With auto set, only a hold that
// is due is locked. It fails with ErrNotParked when s has no such hold.
// fn's transaction commits unless fn fails.
func (p *pgParks) withHold(ctx context.Context, s Stream, auto *autoRedrive, fn func(tx pgx.Tx, redrives int) error) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var (
			redrives int
			err      error
		)
		if auto != nil {
			err = tx.QueryRow(ctx, p.queries.lockDueHold, p.name, s.Tenant, s.ID, auto.now).Scan(&redrives)
		} else {
			err = tx.QueryRow(ctx, p.queries.lockHold, p.name, s.Tenant, s.ID).Scan(&redrives)
		}
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotParked
		case err != nil:
			return fmt.Errorf("lock hold: %w", err)
		}
		return fn(tx, redrives)
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

func (p *pgParks) redriveHead(ctx context.Context, in redriveInput) (headResult, error) {
	var res headResult
	err := p.withHold(ctx, in.stream, in.auto, func(tx pgx.Tx, redrives int) error {
		rows, err := tx.Query(ctx, p.queries.head, p.name, in.stream.Tenant, in.stream.ID)
		if err != nil {
			return fmt.Errorf("read head: %w", err)
		}
		heads, err := pgx.CollectRows(rows, scanHead)
		if err != nil {
			return fmt.Errorf("read head: %w", err)
		}
		if len(heads) == 0 {
			_, err := p.remaining(ctx, tx, in.stream)
			return err
		}
		head := heads[0]
		if res.failure = p.replay(ctx, in.replay, head); res.failure != nil {
			if _, err := tx.Exec(ctx, p.queries.fail, p.name, head.env.EventID, res.failure.Error()); err != nil {
				return fmt.Errorf("record failure: %w", err)
			}
			if err := p.reschedule(ctx, tx, in, redrives+1); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, p.queries.drop, p.name, head.env.EventID); err != nil {
				return fmt.Errorf("release head: %w", err)
			}
			res.replayed = true
		}
		res.remaining, err = p.remaining(ctx, tx, in.stream)
		return err
	})
	return res, err
}

// reschedule moves the schedule of an auto-redriven hold on after its
// redrives-th failure. A manual redrive leaves the schedule alone.
func (p *pgParks) reschedule(ctx context.Context, tx pgx.Tx, in redriveInput, redrives int) error {
	if in.auto == nil {
		return nil
	}
	var next *time.Time
	at, exhausted := in.auto.next(redrives)
	if !exhausted {
		next = &at
	}
	_, err := tx.Exec(ctx, p.queries.reschedule, p.name, in.stream.Tenant, in.stream.ID, redrives, next, exhausted)
	if err != nil {
		return fmt.Errorf("reschedule redrive: %w", err)
	}
	return nil
}

func (p *pgParks) due(ctx context.Context, now time.Time) ([]Stream, error) {
	rows, err := p.pool.Query(ctx, p.queries.due, p.name, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Stream, error) {
		var s Stream
		err := row.Scan(&s.Tenant, &s.ID)
		return s, err
	})
}

func (p *pgParks) nextDue(ctx context.Context) (time.Time, bool, error) {
	var next *time.Time
	if err := p.pool.QueryRow(ctx, p.queries.nextDue, p.name).Scan(&next); err != nil || next == nil {
		return time.Time{}, false, err
	}
	return next.UTC(), true, nil
}

func (p *pgParks) stats(ctx context.Context) (parkStats, error) {
	var st parkStats
	err := p.pool.QueryRow(ctx, p.queries.stats, p.name).Scan(&st.streams, &st.events, &st.exhausted)
	return st, err
}

type pgHead struct {
	env     eventstore.Envelope
	payload eventstore.Payload
}

func scanHead(row pgx.CollectableRow) (pgHead, error) {
	var (
		h         pgHead
		causation *uuid.UUID
	)
	err := row.Scan(&h.env.Cursor.TransactionID, &h.env.Cursor.Position, &h.env.EventID, &h.env.TenantID,
		&h.env.StreamType, &h.env.StreamID, &h.env.StreamPosition, &h.payload.EventType,
		&h.payload.SchemaVersion, &h.payload.Data, &h.env.Actor, &h.env.CorrelationID, &causation,
		&h.env.RecordedAt)
	if err != nil {
		return pgHead{}, err
	}
	h.env.EventType = h.payload.EventType
	h.env.RecordedAt = h.env.RecordedAt.UTC()
	if causation != nil {
		h.env.CausationID = *causation
	}
	return h, nil
}

func (p *pgParks) replay(ctx context.Context, replay replayFunc, h pgHead) error {
	event, err := p.codec.Decode(h.payload)
	if err != nil {
		return fmt.Errorf("event %s: %w", h.env.EventID, err)
	}
	env := h.env
	env.Event = event
	return replay(ctx, env)
}

func (p *pgParks) discard(ctx context.Context, in discardInput) error {
	return p.withHold(ctx, in.stream, nil, func(tx pgx.Tx, _ int) error {
		tag, err := tx.Exec(ctx, p.queries.discard, p.name, in.stream.Tenant, in.stream.ID, in.eventID, in.at, in.by)
		if err != nil {
			return err
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
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Parked, error) {
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
}

// pgQueries are the park list statements for one schema. $1 is always the
// Reactor's name.
type pgQueries struct {
	holdIfParked, park                         string
	lockHold, head, count, release, fail, drop string
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

		lockHold: `SELECT redrives FROM ` + streams + ` WHERE ` + stream + ` FOR UPDATE`,
		lockDueHold: `SELECT redrives FROM ` + streams + ` WHERE ` + stream + `
			AND NOT exhausted AND next_attempt_at <= $4 FOR UPDATE`,
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
			WHERE consumer = $1 AND event_id = $2`,
		drop: `DELETE FROM ` + parked + ` WHERE consumer = $1 AND event_id = $2`,

		discard: `UPDATE ` + parked + ` SET discarded_at = $5, discarded_by = $6
			WHERE ` + stream + ` AND event_id = $4 AND discarded_at IS NULL`,
		list: `SELECT p.tenant_id, p.stream_id, p.stream_type, p.stream_position, p.event_id, p.event_type,
				p.cause, p.attempts, p.parked_at, s.next_attempt_at, s.exhausted
			FROM ` + parked + ` p JOIN ` + streams + ` s USING (consumer, tenant_id, stream_id)
			WHERE p.consumer = $1 AND ($2 = '' OR p.tenant_id = $2) AND p.discarded_at IS NULL
			ORDER BY s.parked_at, p.tenant_id, p.stream_id, p.stream_position`,
	}
}
