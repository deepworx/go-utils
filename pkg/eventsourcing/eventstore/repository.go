package eventstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/aggregateaccess"
	"github.com/deepworx/go-utils/internal/telemetry"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
)

// RepositoryConfig wires a Repository. Store and UnitOfWork must belong to
// the same backend: InMemoryStore with InMemoryUnitOfWork, or the Postgres
// pair.
type RepositoryConfig struct {
	Store      Store
	UnitOfWork UnitOfWork
}

// Repository loads and saves the Aggregates of one type A. P is *A and is
// inferred: NewRepository[TaskID, Task](cfg). The Repository allocates
// Aggregates itself, so the zero value of A is their starting state.
//
// Every method needs a Tenant in ctx (ErrNoTenant). Streams are identified
// by (Tenant, ID).
type Repository[I aggregate.ID, A any, P interface {
	*A
	aggregate.Root[I]
}] struct {
	store      Store
	uow        UnitOfWork
	streamType string
	inst       instruments
}

// NewRepository returns a Repository for A. It panics if cfg lacks a Store
// or UnitOfWork.
//
// Create and Update record an INTERNAL Command span with a PRODUCER send
// span whose traceparent is stored on the appended events. Loads and
// Commands record histograms labelled by stream type. Providers default to
// the OTel globals, so all of it is a no-op until otel.Setup.
func NewRepository[I aggregate.ID, A any, P interface {
	*A
	aggregate.Root[I]
}](cfg RepositoryConfig, opts ...RepositoryOption) *Repository[I, A, P] {
	if cfg.Store == nil || cfg.UnitOfWork == nil {
		panic("eventstore: NewRepository: Store and UnitOfWork are required")
	}
	var o repositoryOptions
	for _, opt := range opts {
		opt(&o)
	}
	streamType := P(new(A)).StreamType()
	return &Repository[I, A, P]{
		store:      cfg.Store,
		uow:        cfg.UnitOfWork,
		streamType: streamType,
		inst:       newInstruments(o.providers, streamType),
	}
}

// Create runs fn on a new, zero-version Aggregate and saves what it raised,
// in one Command transaction. It fails with ErrAlreadyExists when the
// Stream has events, under any stream type. Inside the Command transaction
// for the same Stream it joins it; inside one for another Stream it fails
// with ErrStreamMismatch.
func (r *Repository[I, A, P]) Create(ctx context.Context, id I, fn func(ctx context.Context, agg P) error) (CommandResult, error) {
	var res CommandResult
	err := r.command(ctx, telemetry.SpanCreate(r.streamType), id, func(ctx context.Context) error {
		envs, err := r.loadEnvelopes(ctx, loadInput{stream: uuid.UUID(id)})
		if err != nil {
			return err
		}
		if len(envs) > 0 {
			return ErrAlreadyExists
		}
		agg := r.allocate(id)
		if err := fn(ctx, agg); err != nil {
			return err
		}
		res, err = r.saveCommand(ctx, agg)
		return err
	})
	if err != nil {
		return CommandResult{}, fmt.Errorf("create %s %s: %w", r.streamType, uuid.UUID(id), err)
	}
	return res, nil
}

// UpdateOption configures Update.
type UpdateOption func(*updateOptions)

type updateOptions struct {
	expected int
}

// ExpectVersion makes Update fail with ErrConcurrencyConflict unless the
// Aggregate is at version v, typically the Version of the caller's last
// CommandResult. 0 disables the check.
func ExpectVersion(v int) UpdateOption {
	return func(o *updateOptions) { o.expected = v }
}

// Update loads the Aggregate, checks the Expected version, runs fn and saves
// what it raised, in one Command transaction. If fn raises nothing the
// result has a zero Position. Inside the Command transaction for the same
// Stream it joins it; inside one for another Stream it fails with
// ErrStreamMismatch.
func (r *Repository[I, A, P]) Update(ctx context.Context, id I, fn func(ctx context.Context, agg P) error, opts ...UpdateOption) (CommandResult, error) {
	var o updateOptions
	for _, opt := range opts {
		opt(&o)
	}
	var res CommandResult
	err := r.command(ctx, telemetry.SpanUpdate(r.streamType), id, func(ctx context.Context) error {
		agg, err := r.load(ctx, loadInput{stream: uuid.UUID(id)})
		if err != nil {
			return err
		}
		if o.expected > 0 && agg.Version() != o.expected {
			return fmt.Errorf("at version %d, expected %d: %w", agg.Version(), o.expected, ErrConcurrencyConflict)
		}
		if err := fn(ctx, agg); err != nil {
			return err
		}
		res, err = r.saveCommand(ctx, agg)
		return err
	})
	if err != nil {
		return CommandResult{}, fmt.Errorf("update %s %s: %w", r.streamType, uuid.UUID(id), err)
	}
	return res, nil
}

// Load rebuilds the Aggregate from its whole Stream. It fails with
// ErrNotFound when the Stream has no events or belongs to another stream
// type. Outside a Command transaction it is a plain read without a lock.
func (r *Repository[I, A, P]) Load(ctx context.Context, id I) (P, error) {
	agg, err := r.load(ctx, loadInput{stream: uuid.UUID(id)})
	if err != nil {
		return nil, fmt.Errorf("load %s %s: %w", r.streamType, uuid.UUID(id), err)
	}
	return agg, nil
}

// LoadAt rebuilds the Aggregate as it was at version (>= 1). It fails with
// ErrNotFound when the Stream never reached version.
func (r *Repository[I, A, P]) LoadAt(ctx context.Context, id I, version int) (P, error) {
	agg, err := r.loadAt(ctx, id, version)
	if err != nil {
		return nil, fmt.Errorf("load %s %s at version %d: %w", r.streamType, uuid.UUID(id), version, err)
	}
	return agg, nil
}

func (r *Repository[I, A, P]) loadAt(ctx context.Context, id I, version int) (P, error) {
	if version < 1 {
		return nil, errors.New("version must be at least 1")
	}
	agg, err := r.load(ctx, loadInput{stream: uuid.UUID(id), upTo: version})
	if err != nil {
		return nil, err
	}
	if agg.Version() != version {
		return nil, fmt.Errorf("stream is at version %d: %w", agg.Version(), ErrNotFound)
	}
	return agg, nil
}

// History returns the events of the Stream in stream order. It fails with
// ErrNotFound like Load.
func (r *Repository[I, A, P]) History(ctx context.Context, id I) ([]Envelope, error) {
	envs, err := r.loadOwnEnvelopes(ctx, loadInput{stream: uuid.UUID(id)})
	if err != nil {
		return nil, fmt.Errorf("history %s %s: %w", r.streamType, uuid.UUID(id), err)
	}
	return envs, nil
}

// Save appends the events agg raised since it was loaded. It must run inside
// the Command transaction for agg's Stream (ErrNotCommandTransaction,
// ErrStreamMismatch) and fails with ErrConcurrencyConflict if the Stream
// moved on since agg was loaded. Without raised events it writes nothing
// and returns a zero Position.
func (r *Repository[I, A, P]) Save(ctx context.Context, agg P) (CommandResult, error) {
	res, err := r.save(ctx, agg)
	if err != nil {
		return CommandResult{}, fmt.Errorf("save %s %s: %w", r.streamType, aggregateaccess.UUID[I](agg), err)
	}
	return res, nil
}

// command runs fn in the Command transaction for id, traced as the
// Command span name.
func (r *Repository[I, A, P]) command(ctx context.Context, name string, id I, fn func(ctx context.Context) error) error {
	tenant, _ := ctxutil.TenantID(ctx)
	cmd := commandInput{name: name, tenant: tenant, stream: uuid.UUID(id).String()}
	return r.inst.command(ctx, cmd, func(ctx context.Context) error {
		return r.uow.Within(ctx, uuid.UUID(id), fn)
	})
}

// saveCommand saves agg and annotates the Command span with the event
// types it raised.
func (r *Repository[I, A, P]) saveCommand(ctx context.Context, agg P) (CommandResult, error) {
	raising(ctx, aggregateaccess.Uncommitted[I](agg))
	return r.save(ctx, agg)
}

func (r *Repository[I, A, P]) save(ctx context.Context, agg P) (CommandResult, error) {
	tx, err := commandTxFor(ctx, aggregateaccess.UUID[I](agg))
	if err != nil {
		return CommandResult{}, err
	}
	raised := aggregateaccess.Uncommitted[I](agg)
	version := agg.Version()
	if len(raised) == 0 {
		return CommandResult{Version: version}, nil
	}
	var pos Cursor
	err = r.inst.send(ctx, func(ctx context.Context, traceContext string) error {
		pos, err = r.store.append(ctx, appendInput{
			tx:           tx,
			streamType:   r.streamType,
			expected:     version - len(raised),
			events:       raised,
			traceContext: traceContext,
		})
		return err
	})
	if err != nil {
		return CommandResult{}, err
	}
	aggregateaccess.MarkCommitted[I](agg)
	return CommandResult{Position: pos, Version: version}, nil
}

func (r *Repository[I, A, P]) allocate(id I) P {
	agg := P(new(A))
	aggregateaccess.Init[I](agg, id)
	return agg
}

func (r *Repository[I, A, P]) load(ctx context.Context, in loadInput) (P, error) {
	var envs []Envelope
	err := r.inst.load(ctx, func() (int, error) {
		var err error
		envs, err = r.loadOwnEnvelopes(ctx, in)
		return len(envs), err
	})
	if err != nil {
		return nil, err
	}
	agg := r.allocate(I(in.stream))
	for _, env := range envs {
		aggregateaccess.Replay[I](agg, env.Event.(aggregate.Event))
	}
	return agg, nil
}

// loadOwnEnvelopes loads a Stream that must exist under r's stream type.
func (r *Repository[I, A, P]) loadOwnEnvelopes(ctx context.Context, in loadInput) ([]Envelope, error) {
	envs, err := r.loadEnvelopes(ctx, in)
	if err != nil {
		return nil, err
	}
	if len(envs) == 0 {
		return nil, ErrNotFound
	}
	if got := envs[0].StreamType; got != r.streamType {
		return nil, fmt.Errorf("stream has type %s: %w", got, ErrNotFound)
	}
	return envs, nil
}

func (r *Repository[I, A, P]) loadEnvelopes(ctx context.Context, in loadInput) ([]Envelope, error) {
	tenant, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	in.tenant = tenant
	return r.store.load(ctx, in)
}
