package eventstore

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/postgres"
)

// PgUnitOfWorkConfig configures a PgUnitOfWork.
type PgUnitOfWorkConfig struct {
	// UnitOfWork runs the Postgres transactions; its options (TxOptions,
	// SetLocalTenant) apply to every Command transaction. Required.
	UnitOfWork *postgres.PgUnitOfWork
	// Schema is part of every stream lock key, so deployments in different
	// schemas of one database never block each other. It must match the
	// PgStore's Schema. Defaults to public.
	Schema string
}

// PgUnitOfWork runs Command transactions in Postgres. Each one takes
// pg_advisory_xact_lock on a key derived from (Schema, Tenant, Stream)
// before fn runs, so writers to one Stream serialize across connections and
// processes until commit. Guard, numbering and other postgres.Conn users in
// fn join the transaction.
//
// Command transactions must run at READ COMMITTED, so every read after the
// lock sees what earlier writers committed; any other isolation level fails
// with ErrUnsupportedIsolation (ADR-0013). The lock serializes only writers
// to this Stream: reads of other data are as fresh as READ COMMITTED, no
// more.
type PgUnitOfWork struct {
	uow    *postgres.PgUnitOfWork
	schema string
}

// NewPgUnitOfWork returns a PgUnitOfWork. It fails with ErrInvalidConfig
// without UnitOfWork, and with notify.ErrInvalidChannel when Schema makes
// the derived channel name too long.
func NewPgUnitOfWork(cfg PgUnitOfWorkConfig) (*PgUnitOfWork, error) {
	if cfg.UnitOfWork == nil {
		return nil, fmt.Errorf("eventstore: NewPgUnitOfWork: UnitOfWork is required: %w", ErrInvalidConfig)
	}
	schema, err := checkSchema(cfg.Schema)
	if err != nil {
		return nil, err
	}
	return &PgUnitOfWork{uow: cfg.UnitOfWork, schema: schema}, nil
}

// readCommitted is the only transaction_isolation a Command transaction
// accepts.
const readCommitted = "read committed"

// holdStream takes the stream lock $1 and records the held Stream $2 in a
// transaction-local setting, in one round trip. It returns the Stream the
// database transaction already held, or $2 when it held none, and the
// transaction's isolation level.
const holdStream = `SELECT coalesce(nullif(current_setting('eventsourcing.command_stream', true), ''),
		set_config('eventsourcing.command_stream', $2, true)),
		current_setting('transaction_isolation')
	FROM pg_advisory_xact_lock($1)`

// Execute opens a Command transaction for stream, waiting for its stream
// lock until ctx ends. An open Postgres transaction in ctx is joined, and
// the lock is then held until that transaction ends. One database
// transaction holds one Stream: joining one that already holds another
// Stream fails with ErrStreamMismatch. A transaction that does not run at
// READ COMMITTED fails with ErrUnsupportedIsolation once it holds the lock.
func (u *PgUnitOfWork) Execute(ctx context.Context, stream uuid.UUID, fn func(ctx context.Context) error) error {
	ctx, tx, err := openCommandTx(ctx, stream)
	if err != nil {
		return err
	}
	ref := streamRef{schema: u.schema, tenant: tx.tenant, stream: stream}
	return u.uow.Execute(ctx, func(ctx context.Context) error {
		var held, isolation string
		err := postgres.TxFromContext(ctx).QueryRow(ctx, holdStream, ref.lockKey(), ref.String()).Scan(&held, &isolation)
		if err != nil {
			return fmt.Errorf("lock stream %s: %w", stream, err)
		}
		if held != ref.String() {
			return fmt.Errorf("database transaction holds %s, got %s: %w", held, ref, ErrStreamMismatch)
		}
		if isolation != readCommitted {
			return fmt.Errorf("command transaction runs %s, need %s: %w", isolation, readCommitted, ErrUnsupportedIsolation)
		}
		return fn(ctx)
	})
}

// Within joins the open Command transaction for stream, or opens one.
func (u *PgUnitOfWork) Within(ctx context.Context, stream uuid.UUID, fn func(ctx context.Context) error) error {
	return executor(u.Execute).within(ctx, stream, fn)
}

// streamRef names a Stream of one Tenant in one schema.
type streamRef struct {
	schema string
	tenant string
	stream uuid.UUID
}

// String renders r unambiguously, as the held Stream of a transaction.
func (r streamRef) String() string {
	return strconv.Quote(r.schema) + "/" + strconv.Quote(r.tenant) + "/" + r.stream.String()
}

// lockKey is fnv64a(schema ‖ 0x00 ‖ tenant ‖ 0x00 ‖ stream), with stream in
// its canonical text form, as a bigint advisory lock key.
func (r streamRef) lockKey() int64 {
	h := fnv.New64a()
	for i, part := range []string{r.schema, r.tenant, r.stream.String()} {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(part))
	}
	return int64(h.Sum64()) //nolint:gosec // the bit pattern is the key; overflow is intended
}

var _ UnitOfWork = (*PgUnitOfWork)(nil)
