// PROTOTYPE (DWX-212): throwaway API sketch, not production code.
package eventstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/aggregate"
)

// RepositoryConfig wires a Repository.
type RepositoryConfig struct {
	Store      *InMemoryStore      // real: Store interface (PgStore / InMemoryStore)
	UnitOfWork *InMemoryUnitOfWork // real: UnitOfWork interface (DWX-209)
}

// Repository loads and saves one aggregate type A (P is *A, inferred).
// It allocates aggregates itself, so the domain exports no empty constructor.
type Repository[I aggregate.ID, A any, P interface {
	*A
	aggregate.Root[I]
}] struct {
	cfg RepositoryConfig
}

// NewRepository returns a Repository: NewRepository[todo.TaskID, todo.Task](cfg).
func NewRepository[I aggregate.ID, A any, P interface {
	*A
	aggregate.Root[I]
}](cfg RepositoryConfig) *Repository[I, A, P] {
	return &Repository[I, A, P]{cfg: cfg}
}

func (r *Repository[I, A, P]) empty(id I) P {
	p := P(new(A))
	aggregate.Init[I](p, id)
	return p
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
func (r *Repository[I, A, P]) Update(ctx context.Context, id I, fn func(ctx context.Context, agg P) error, opts ...UpdateOption) (CommandResult, error) {
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
// command tx (lock) → assert empty (ErrAlreadyExists) → fn on a fresh,
// zero-version aggregate → Save.
func (r *Repository[I, A, P]) Create(ctx context.Context, id I, fn func(ctx context.Context, agg P) error) (CommandResult, error) {
	var res CommandResult
	err := r.cfg.UnitOfWork.Execute(ctx, uuid.UUID(id), func(ctx context.Context) error {
		agg, err := r.Load(ctx, id)
		switch {
		case err == nil:
			return fmt.Errorf("stream %s: %w", uuid.UUID(id), ErrAlreadyExists)
		case !errors.Is(err, ErrNotFound):
			return err
		}
		agg = r.empty(id)
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
func (r *Repository[I, A, P]) Load(ctx context.Context, id I) (P, error) {
	return r.loadTo(ctx, id, 0)
}

// LoadAt replays the stream up to and including version (CPQ's LoadAt).
// ErrNotFound if the stream never reached version.
func (r *Repository[I, A, P]) LoadAt(ctx context.Context, id I, version int) (P, error) {
	var zero P
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

func (r *Repository[I, A, P]) loadTo(ctx context.Context, id I, to int) (P, error) {
	var zero P
	t, err := tenant(ctx)
	if err != nil {
		return zero, err
	}
	envs := r.cfg.Store.load(streamKey{tenant: t, id: uuid.UUID(id)}, to)
	if len(envs) == 0 {
		return zero, fmt.Errorf("stream %s: %w", uuid.UUID(id), ErrNotFound)
	}
	agg := r.empty(id)
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
func (r *Repository[I, A, P]) Save(ctx context.Context, agg P) (CommandResult, error) {
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
