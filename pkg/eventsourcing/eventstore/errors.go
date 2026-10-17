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
