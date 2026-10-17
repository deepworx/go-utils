// PROTOTYPE (DWX-212): throwaway API sketch, not production code.
package eventstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/aggregate"
)

// RepositoryConfig wires a Repository (CS-5: more than two inputs).
type RepositoryConfig[T aggregate.Root[I], I aggregate.ID] struct {
	Store      *InMemoryStore      // real: Store interface (PgStore / InMemoryStore)
	UnitOfWork *InMemoryUnitOfWork // real: UnitOfWork interface (DWX-209)
	// New returns a fresh zero-version aggregate to replay history into.
	New func(id I) T
}

// Repository loads and saves one aggregate type.
type Repository[T aggregate.Root[I], I aggregate.ID] struct {
	cfg RepositoryConfig[T, I]
}

// NewRepository returns a Repository. Type parameters are inferred from New.
func NewRepository[T aggregate.Root[I], I aggregate.ID](cfg RepositoryConfig[T, I]) *Repository[T, I] {
	return &Repository[T, I]{cfg: cfg}
}

// --- high level: one call per command ---------------------------------------

type updateOptions struct{ expected int }

// UpdateOption configures Update.
type UpdateOption func(*updateOptions)

// ExpectVersion makes Update fail with ErrConcurrencyConflict unless the
// loaded aggregate is at version v (the caller's last CommandResult.Version).
func ExpectVersion(v int) UpdateOption {
	return func(o *updateOptions) { o.expected = v }
}

// Update runs one command against an existing aggregate:
// command tx (lock) → Load → expected-version check → fn → Save.
// fn gets the tx ctx, so guards/numbering inside it join the command tx.
func (r *Repository[T, I]) Update(ctx context.Context, id I, fn func(ctx context.Context, agg T) error, opts ...UpdateOption) (CommandResult, error) {
	var o updateOptions
	for _, opt := range opts {
		opt(&o)
	}
	var res CommandResult
	err := r.cfg.UnitOfWork.Execute(ctx, uuid.UUID(id), func(ctx context.Context) error {
		agg, err := r.Load(ctx, id)
		if err != nil {
			return err
		}
		if o.expected > 0 && agg.Version() != o.expected {
			return fmt.Errorf("stream %s at version %d, expected %d: %w",
				uuid.UUID(id), agg.Version(), o.expected, ErrConcurrencyConflict)
		}
		if err := fn(ctx, agg); err != nil {
			return err
		}
		res, err = r.Save(ctx, agg)
		return err
	})
	return res, err
}

// Create runs one command that starts a new stream:
// command tx (lock) → assert empty → fn on New(id) → Save.
func (r *Repository[T, I]) Create(ctx context.Context, id I, fn func(ctx context.Context, agg T) error) (CommandResult, error) {
	var res CommandResult
	err := r.cfg.UnitOfWork.Execute(ctx, uuid.UUID(id), func(ctx context.Context) error {
		agg, err := r.Load(ctx, id)
		switch {
		case err == nil:
			return fmt.Errorf("stream %s: %w", uuid.UUID(id), ErrAlreadyExists)
		case !errors.Is(err, ErrNotFound):
			return err
		}
		agg = r.cfg.New(id)
		if err := fn(ctx, agg); err != nil {
			return err
		}
		res, err = r.Save(ctx, agg)
		return err
	})
	return res, err
}

// --- low level: for handlers that need more than load-decide-save ----------

// Load replays the whole stream. ErrNotFound if it has no events. Works
// inside or outside a command transaction; outside, it is a plain read.
func (r *Repository[T, I]) Load(ctx context.Context, id I) (T, error) {
	return r.loadTo(ctx, id, 0)
}

// LoadAt replays the stream up to and including version (CPQ's LoadAt).
// ErrNotFound if the stream never reached version.
func (r *Repository[T, I]) LoadAt(ctx context.Context, id I, version int) (T, error) {
	var zero T
	if version < 1 {
		return zero, fmt.Errorf("load %s at version %d: version must be >= 1", uuid.UUID(id), version)
	}
	agg, err := r.loadTo(ctx, id, version)
	if err != nil {
		return zero, err
	}
	if v := agg.Version(); v != version {
		return zero, fmt.Errorf("stream %s reached version %d, asked for %d: %w", uuid.UUID(id), v, version, ErrNotFound)
	}
	return agg, nil
}

func (r *Repository[T, I]) loadTo(ctx context.Context, id I, to int) (T, error) {
	var zero T
	t, err := tenant(ctx)
	if err != nil {
		return zero, err
	}
	envs := r.cfg.Store.load(streamKey{tenant: t, id: uuid.UUID(id)}, to)
	if len(envs) == 0 {
		return zero, fmt.Errorf("stream %s: %w", uuid.UUID(id), ErrNotFound)
	}
	agg := r.cfg.New(id)
	for _, env := range envs {
		ev, err := r.cfg.Store.codec.decode(env.EventType, env.Data)
		if err != nil {
			return zero, fmt.Errorf("load %s: %w", uuid.UUID(id), err)
		}
		aggregate.Replay(agg, ev)
	}
	return agg, nil
}

// Save appends uncommitted events. Must run inside the command transaction
// for agg's stream (ErrNotCommandTransaction / ErrStreamMismatch otherwise).
func (r *Repository[T, I]) Save(ctx context.Context, agg T) (CommandResult, error) {
	id := aggregate.UUID(agg)
	k, err := commandStream(ctx, id)
	if err != nil {
		return CommandResult{}, fmt.Errorf("save %s: %w", id, err)
	}
	pending := aggregate.Uncommitted(agg)
	version := agg.Version()
	if len(pending) == 0 {
		return CommandResult{Version: version}, nil
	}
	pos, err := r.cfg.Store.append(appendInput{
		key: k, streamType: agg.StreamType(), priorVersion: version - len(pending), events: pending,
	})
	if err != nil {
		return CommandResult{}, fmt.Errorf("save %s: %w", id, err)
	}
	aggregate.MarkCommitted(agg)
	return CommandResult{Position: pos, Version: version}, nil
}
