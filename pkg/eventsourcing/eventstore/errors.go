package eventstore

import "errors"

var (
	// ErrUnknownEventType reports an Event type that is not registered on
	// the Codec, or an event whose Go type differs from the registered one.
	ErrUnknownEventType = errors.New("eventstore: unknown event type")
	// ErrNewerSchemaVersion reports a stored payload whose Schema version is
	// newer than the registered one, typically after a rollback.
	ErrNewerSchemaVersion = errors.New("eventstore: schema version newer than registered")
	// ErrInvalidSchemaVersion reports a stored Schema version below 1.
	ErrInvalidSchemaVersion = errors.New("eventstore: invalid schema version")
)

var (
	// ErrNotFound reports a Stream without events, or one that exists under
	// another stream type than the Repository's.
	ErrNotFound = errors.New("eventstore: stream not found")
	// ErrAlreadyExists reports a Create for a Stream that already has
	// events, under any stream type.
	ErrAlreadyExists = errors.New("eventstore: stream already exists")
	// ErrConcurrencyConflict reports a stale Expected version, or a Save of
	// an Aggregate whose Stream moved on since it was loaded.
	ErrConcurrencyConflict = errors.New("eventstore: concurrency conflict")
	// ErrNotCommandTransaction reports a Save outside a Command transaction.
	ErrNotCommandTransaction = errors.New("eventstore: not in a command transaction")
	// ErrStreamMismatch reports touching a Stream other than the one the
	// open Command transaction holds.
	ErrStreamMismatch = errors.New("eventstore: command transaction holds another stream")
	// ErrNestedUnitOfWork reports UnitOfWork.Execute inside an open Command
	// transaction. Use Within to join it.
	ErrNestedUnitOfWork = errors.New("eventstore: nested command transaction")
	// ErrNoTenant reports a context without a Tenant (ctxutil.TenantID).
	ErrNoTenant = errors.New("eventstore: no tenant in context")
	// ErrNoActor reports an append whose context has neither an Actor
	// (ctxutil.WithActor) nor a Claims.UserID outside the reserved
	// "system:" prefix.
	ErrNoActor = errors.New("eventstore: no actor in context")
	// ErrInvalidConfig reports a constructor Config that misses a required
	// dependency. Constructors without an error result panic instead.
	ErrInvalidConfig = errors.New("eventstore: invalid config")
	// ErrInvalidVersion reports a requested Aggregate version below 1.
	ErrInvalidVersion = errors.New("eventstore: invalid version")
)
