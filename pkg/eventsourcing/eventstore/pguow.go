package eventstore

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

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
type PgUnitOfWork struct {
	uow    *postgres.PgUnitOfWork
	schema string
}

// NewPgUnitOfWork returns a PgUnitOfWork. It fails without UnitOfWork, and
// with notify.ErrInvalidChannel when Schema makes the derived channel name
// too long.
func NewPgUnitOfWork(cfg PgUnitOfWorkConfig) (*PgUnitOfWork, error) {
	if cfg.UnitOfWork == nil {
		return nil, errors.New("eventstore: NewPgUnitOfWork: UnitOfWork is required")
	}
	schema, err := checkSchema(cfg.Schema)
	if err != nil {
		return nil, err
	}
	return &PgUnitOfWork{uow: cfg.UnitOfWork, schema: schema}, nil
}

// Execute opens a Command transaction for stream, waiting for its stream
// lock until ctx ends. An open Postgres transaction in ctx is joined, and
// the lock is then held until that transaction ends.
func (u *PgUnitOfWork) Execute(ctx context.Context, stream uuid.UUID, fn func(ctx context.Context) error) error {
	ctx, tx, err := openCommandTx(ctx, stream)
	if err != nil {
		return err
	}
	return u.uow.Execute(ctx, func(ctx context.Context) error {
		pgTx := postgres.TxFromContext(ctx)
		key := streamLockKey(u.schema, tx.tenant, stream)
		if _, err := pgTx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
			return fmt.Errorf("lock stream %s: %w", stream, err)
		}
		return fn(ctx)
	})
}

// Within joins the open Command transaction for stream, or opens one.
func (u *PgUnitOfWork) Within(ctx context.Context, stream uuid.UUID, fn func(ctx context.Context) error) error {
	return executor(u.Execute).within(ctx, stream, fn)
}

// streamLockKey is fnv64a(schema ‖ 0x00 ‖ tenant ‖ 0x00 ‖ stream), with
// stream in its canonical text form, as a bigint advisory lock key.
func streamLockKey(schema, tenant string, stream uuid.UUID) int64 {
	h := fnv.New64a()
	for i, part := range []string{schema, tenant, stream.String()} {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(part))
	}
	return int64(h.Sum64()) //nolint:gosec // the bit pattern is the key; overflow is intended
}

var _ UnitOfWork = (*PgUnitOfWork)(nil)
