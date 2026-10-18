package eventstore

import (
	"context"
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

// NewRepository returns a Repository for A. A missing Store or UnitOfWork
// is a programming error, so it panics instead of returning
// ErrInvalidConfig.
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
func (r *Repository[I, A, P]) Create(ctx context.Context, id I, fn func(ctx context.Context, agg P) error, opts ...CommandOption) (CommandResult, error) {
	create := func(ctx context.Context) (CommandResult, error) {
		envs, err := r.loadEnvelopes(ctx, loadInput{stream: uuid.UUID(id)})
		if err != nil {
			return CommandResult{}, err
		}
		if len(envs) > 0 {
			return CommandResult{}, ErrAlreadyExists
		}
		agg := r.allocate(id)
		if err := fn(ctx, agg); err != nil {
			return CommandResult{}, err
		}
		return r.saveCommand(ctx, agg)
	}
	call := commandCall[I]{span: telemetry.SpanCreate(r.streamType), id: id, opts: newCommandOptions(opts), fn: create}
	res, err := r.command(ctx, call)
	if err != nil {
		return CommandResult{}, fmt.Errorf("create %s %s: %w", r.streamType, uuid.UUID(id), err)
	}
	return res, nil
}

// CommandOption configures Create and Update.
type CommandOption func(*commandOptions)

type commandOptions struct {
	expected  int
	commandID uuid.UUID
}

func newCommandOptions(opts []CommandOption) commandOptions {
	var o commandOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// ExpectVersion makes Update fail with ErrConcurrencyConflict unless the
// Aggregate is at version v, typically the Version of the caller's last
// CommandResult. 0 disables the check. Create always expects an empty
// Stream and ignores it.
func ExpectVersion(v int) CommandOption {
	return func(o *commandOptions) { o.expected = v }
}

// WithCommandID makes the command take effect at most once per Tenant
// (ADR-0012). A repeat with the same id returns the original CommandResult
// with Replayed set, without running fn and before the Expected version is
// checked. The same id on another Stream fails with ErrCommandIDReused.
// The id is recorded in the Command transaction, also when fn raises
// nothing; a rejected command records nothing. uuid.Nil disables the
// dedupe. An id recorded concurrently fails with ErrConcurrencyConflict;
// on Postgres that aborts the whole Command transaction, including a
// caller's outer one, since no savepoint guards the insert.
func WithCommandID(id uuid.UUID) CommandOption {
	return func(o *commandOptions) { o.commandID = id }
}

// Update loads the Aggregate, checks the Expected version, runs fn and saves
// what it raised, in one Command transaction. If fn raises nothing the
// result has a zero Position. Inside the Command transaction for the same
// Stream it joins it; inside one for another Stream it fails with
// ErrStreamMismatch.
func (r *Repository[I, A, P]) Update(ctx context.Context, id I, fn func(ctx context.Context, agg P) error, opts ...CommandOption) (CommandResult, error) {
	o := newCommandOptions(opts)
	update := func(ctx context.Context) (CommandResult, error) {
		agg, err := r.load(ctx, loadInput{stream: uuid.UUID(id)})
		if err != nil {
			return CommandResult{}, err
		}
		if o.expected > 0 && agg.Version() != o.expected {
			return CommandResult{}, fmt.Errorf("at version %d, expected %d: %w", agg.Version(), o.expected, ErrConcurrencyConflict)
		}
		if err := fn(ctx, agg); err != nil {
			return CommandResult{}, err
		}
		return r.saveCommand(ctx, agg)
	}
	call := commandCall[I]{span: telemetry.SpanUpdate(r.streamType), id: id, opts: o, fn: update}
	res, err := r.command(ctx, call)
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

// LoadAt rebuilds the Aggregate as it was at version. It fails with
// ErrInvalidVersion for a version below 1 and with ErrNotFound when the
// Stream never reached version.
func (r *Repository[I, A, P]) LoadAt(ctx context.Context, id I, version int) (P, error) {
	agg, err := r.loadAt(ctx, id, version)
	if err != nil {
		return nil, fmt.Errorf("load %s %s at version %d: %w", r.streamType, uuid.UUID(id), version, err)
	}
	return agg, nil
}

func (r *Repository[I, A, P]) loadAt(ctx context.Context, id I, version int) (P, error) {
	if version < 1 {
		return nil, ErrInvalidVersion
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

// commandCall is one Command: fn runs in the Command transaction for id,
// traced as the Command span named span.
type commandCall[I aggregate.ID] struct {
	span string
	id   I
	opts commandOptions
	fn   func(ctx context.Context) (CommandResult, error)
}

// command runs call in its Command transaction.
func (r *Repository[I, A, P]) command(ctx context.Context, call commandCall[I]) (CommandResult, error) {
	tenant, _ := ctxutil.TenantID(ctx)
	stream := uuid.UUID(call.id)
	cmd := commandInput{name: call.span, tenant: tenant, stream: stream.String()}
	return r.inst.command(ctx, cmd, func(ctx context.Context) (CommandResult, error) {
		var res CommandResult
		err := r.uow.Within(ctx, stream, func(ctx context.Context) error {
			var err error
			res, err = r.deduplicated(ctx, call)
			return err
		})
		return res, err
	})
}

// deduplicated runs call.fn unless its Command ID is already recorded for the
// Tenant, and records the Command ID with the result in the same Command
// transaction.
func (r *Repository[I, A, P]) deduplicated(ctx context.Context, call commandCall[I]) (CommandResult, error) {
	if call.opts.commandID == uuid.Nil {
		return call.fn(ctx)
	}
	tx, err := commandTxFor(ctx, uuid.UUID(call.id))
	if err != nil {
		return CommandResult{}, err
	}
	prior, found, err := r.store.findCommand(ctx, tx, call.opts.commandID)
	if err != nil {
		return CommandResult{}, err
	}
	if found {
		if prior.stream != tx.stream {
			return CommandResult{}, fmt.Errorf("command %s ran on stream %s: %w", prior.id, prior.stream, ErrCommandIDReused)
		}
		res := prior.result
		res.Replayed = true
		return res, nil
	}
	res, err := call.fn(ctx)
	if err != nil {
		return CommandResult{}, err
	}
	err = r.store.recordCommand(ctx, tx, processedCommand{id: call.opts.commandID, stream: tx.stream, result: res})
	if err != nil {
		return CommandResult{}, err
	}
	return res, nil
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
		e, ok := env.Event.(aggregate.Event)
		if !ok {
			return nil, fmt.Errorf("replay position %d: %T is no aggregate.Event: %w", env.StreamPosition, env.Event, ErrUnknownEventType)
		}
		aggregateaccess.Replay[I](agg, e)
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
