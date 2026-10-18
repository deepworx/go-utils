package eventstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

const defaultSchema = "public"

// streamPositionKey is the unique constraint on (tenant_id, stream_id,
// stream_position) in docs/postgres/schema.sql. Hosts must keep it under
// this name (ADR-0003).
const streamPositionKey = "events_stream_position_key"

// processedCommandsKey is the primary key on (tenant_id, command_id) in
// docs/postgres/schema.sql.
const processedCommandsKey = "processed_commands_pkey"

// errNoPgTransaction reports a PgStore write outside a PgUnitOfWork.
var errNoPgTransaction = errors.New("eventstore: PgStore writes only inside a PgUnitOfWork")

// checkSchema applies the default schema and rejects one whose derived
// channel <schema>.events is not a valid notify channel.
func checkSchema(schema string) (string, error) {
	if schema == "" {
		schema = defaultSchema
	}
	if err := notify.CheckChannel(schema + ".events"); err != nil {
		return "", fmt.Errorf("eventstore: schema %q: %w", schema, err)
	}
	return schema, nil
}

// PgStoreConfig configures a PgStore.
type PgStoreConfig struct {
	// Pool runs loads outside a transaction. Required.
	Pool *pgxpool.Pool
	// Codec encodes and decodes the events. Required.
	Codec *Codec
	// Schema qualifies the events and processed_commands tables; the events
	// trigger notifies <schema>.events. Defaults to public.
	Schema string
}

// PgStore is the event log in the events table of docs/postgres/schema.sql,
// with the Command IDs in processed_commands. It writes only inside a
// Command transaction of a PgUnitOfWork.
type PgStore struct {
	pool    *pgxpool.Pool
	schema  string
	codec   *Codec
	clock   func() time.Time
	queries pgQueries
}

type pgQueries struct {
	load            string
	version         string
	insert          string
	findCommand     string
	recordCommand   string
	cleanupCommands string
}

func newPgQueries(schema string) pgQueries {
	table := pgx.Identifier{schema, "events"}.Sanitize()
	commands := pgx.Identifier{schema, "processed_commands"}.Sanitize()
	return pgQueries{
		load: `SELECT transaction_id::text::numeric, global_position, event_id, stream_type, stream_position,
				event_type, schema_version, event_data, actor, correlation_id, causation_id, recorded_at
			FROM ` + table + `
			WHERE tenant_id = $1 AND stream_id = $2 AND ($3::int = 0 OR stream_position <= $3)
			ORDER BY stream_position`,
		version: `SELECT COALESCE(max(stream_position), 0) FROM ` + table + `
			WHERE tenant_id = $1 AND stream_id = $2`,
		insert: `INSERT INTO ` + table + ` (event_id, tenant_id, stream_type, stream_id, stream_position,
				event_type, schema_version, event_data, actor, correlation_id, causation_id, trace_context, recorded_at)
			SELECT e.event_id, $1, $2, $3, $4 + e.ord, e.event_type, e.schema_version, e.event_data,
				$5, $6, $7, NULLIF($13, ''), $8
			FROM unnest($9::uuid[], $10::text[], $11::int[], $12::bytea[]) WITH ORDINALITY
				AS e(event_id, event_type, schema_version, event_data, ord)
			ORDER BY e.ord
			RETURNING transaction_id::text::numeric, global_position`,
		findCommand: `SELECT stream_id, transaction_id::text::numeric, position, version FROM ` + commands + `
			WHERE tenant_id = $1 AND command_id = $2`,
		recordCommand: `INSERT INTO ` + commands + ` (tenant_id, command_id, stream_id, transaction_id, position, version, recorded_at)
			VALUES ($1, $2, $3, $4::numeric::text::xid8, $5, $6, $7)`,
		cleanupCommands: `DELETE FROM ` + commands + ` WHERE recorded_at < $1`,
	}
}

// NewPgStore returns a PgStore. It fails with ErrInvalidConfig when Pool or
// Codec is missing, and with notify.ErrInvalidChannel when Schema makes the
// derived channel name too long.
func NewPgStore(cfg PgStoreConfig, opts ...StoreOption) (*PgStore, error) {
	if cfg.Pool == nil || cfg.Codec == nil {
		return nil, fmt.Errorf("eventstore: NewPgStore: Pool and Codec are required: %w", ErrInvalidConfig)
	}
	schema, err := checkSchema(cfg.Schema)
	if err != nil {
		return nil, err
	}
	return &PgStore{
		pool:    cfg.Pool,
		schema:  schema,
		codec:   cfg.Codec,
		clock:   newStoreOptions(opts).clock,
		queries: newPgQueries(schema),
	}, nil
}

func (s *PgStore) load(ctx context.Context, in loadInput) ([]Envelope, error) {
	rows, err := postgres.Conn(ctx, s.pool).Query(ctx, s.queries.load, in.tenant, in.stream, in.upTo)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", in.stream, err)
	}
	envs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Envelope, error) {
		return s.scanEnvelope(row, in)
	})
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", in.stream, err)
	}
	return envs, nil
}

func (s *PgStore) scanEnvelope(row pgx.CollectableRow, in loadInput) (Envelope, error) {
	env := Envelope{TenantID: in.tenant, StreamID: in.stream}
	var (
		payload   Payload
		causation *uuid.UUID
	)
	err := row.Scan(&env.Cursor.TransactionID, &env.Cursor.Position, &env.EventID, &env.StreamType,
		&env.StreamPosition, &payload.EventType, &payload.SchemaVersion, &payload.Data,
		&env.Actor, &env.CorrelationID, &causation, &env.RecordedAt)
	if err != nil {
		return Envelope{}, err
	}
	if env.Event, err = s.codec.Decode(payload); err != nil {
		return Envelope{}, fmt.Errorf("position %d: %w", env.StreamPosition, err)
	}
	env.EventType = payload.EventType
	env.RecordedAt = env.RecordedAt.UTC()
	if causation != nil {
		env.CausationID = *causation
	}
	return env, nil
}

func (s *PgStore) append(ctx context.Context, in appendInput) (Cursor, error) {
	tx := postgres.TxFromContext(ctx)
	if tx == nil {
		return Cursor{}, errNoPgTransaction
	}
	meta, err := resolveMetadata(ctx, in.tx)
	if err != nil {
		return Cursor{}, err
	}
	batch, err := s.encode(in.events)
	if err != nil {
		return Cursor{}, err
	}

	var version int
	if err := tx.QueryRow(ctx, s.queries.version, in.tx.tenant, in.tx.stream).Scan(&version); err != nil {
		return Cursor{}, fmt.Errorf("read version of %s: %w", in.tx.stream, err)
	}
	if version != in.expected {
		return Cursor{}, fmt.Errorf("stream %s at version %d, expected %d: %w", in.tx.stream, version, in.expected, ErrConcurrencyConflict)
	}

	var causation *uuid.UUID
	if meta.causation != uuid.Nil {
		causation = &meta.causation
	}
	rows, err := tx.Query(ctx, s.queries.insert,
		in.tx.tenant, in.streamType, in.tx.stream, in.expected,
		meta.actor, meta.correlation, causation, s.clock(),
		batch.ids, batch.types, batch.versions, batch.data, in.traceContext)
	if err != nil {
		return Cursor{}, fmt.Errorf("append to %s: %w", in.tx.stream, err)
	}
	cursors, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Cursor])
	if err != nil {
		return Cursor{}, fmt.Errorf("append to %s: %w", in.tx.stream, asConflict(err))
	}
	return cursors[len(cursors)-1], nil
}

func (s *PgStore) findCommand(ctx context.Context, tx *commandTx, id uuid.UUID) (processedCommand, bool, error) {
	cmd := processedCommand{id: id}
	pos := &cmd.result.Position
	err := postgres.Conn(ctx, s.pool).QueryRow(ctx, s.queries.findCommand, tx.tenant, id).
		Scan(&cmd.stream, &pos.TransactionID, &pos.Position, &cmd.result.Version)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return processedCommand{}, false, nil
	case err != nil:
		return processedCommand{}, false, fmt.Errorf("find command %s: %w", id, err)
	}
	return cmd, true, nil
}

func (s *PgStore) recordCommand(ctx context.Context, tx *commandTx, cmd processedCommand) error {
	pgTx := postgres.TxFromContext(ctx)
	if pgTx == nil {
		return errNoPgTransaction
	}
	pos := cmd.result.Position
	_, err := pgTx.Exec(ctx, s.queries.recordCommand,
		tx.tenant, cmd.id, cmd.stream, pos.TransactionID, pos.Position, cmd.result.Version, s.clock())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == processedCommandsKey {
		return fmt.Errorf("command %s recorded concurrently: %w: %w", cmd.id, ErrConcurrencyConflict, err)
	}
	if err != nil {
		return fmt.Errorf("record command %s: %w", cmd.id, err)
	}
	return nil
}

// CleanupCommands deletes the rows of processed_commands recorded before
// the cutoff, for all Tenants. It joins a transaction in ctx. A repeat of a
// deleted Command ID runs as a new command.
func (s *PgStore) CleanupCommands(ctx context.Context, before time.Time) (int64, error) {
	tag, err := postgres.Conn(ctx, s.pool).Exec(ctx, s.queries.cleanupCommands, before)
	if err != nil {
		return 0, fmt.Errorf("cleanup commands before %s: %w", before.Format(time.RFC3339), err)
	}
	return tag.RowsAffected(), nil
}

func (s *PgStore) now() time.Time { return s.clock() }

// pgBatch holds the per-event columns of an append as parallel arrays.
type pgBatch struct {
	ids      []uuid.UUID
	types    []string
	versions []int
	data     [][]byte
}

func (s *PgStore) encode(events []aggregate.Event) (pgBatch, error) {
	b := pgBatch{
		ids:      make([]uuid.UUID, len(events)),
		types:    make([]string, len(events)),
		versions: make([]int, len(events)),
		data:     make([][]byte, len(events)),
	}
	for i, e := range events {
		p, err := s.codec.Encode(e)
		if err != nil {
			return pgBatch{}, err
		}
		if b.ids[i], err = uuid.NewV7(); err != nil {
			return pgBatch{}, fmt.Errorf("generate event id: %w", err)
		}
		b.types[i], b.versions[i], b.data[i] = p.EventType, p.SchemaVersion, p.Data
	}
	return b, nil
}

// asConflict maps a violated stream position to ErrConcurrencyConflict. It
// only happens when a writer bypassed the stream lock.
func asConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == streamPositionKey {
		return fmt.Errorf("%w: %w", ErrConcurrencyConflict, err)
	}
	return err
}

var _ Store = (*PgStore)(nil)
