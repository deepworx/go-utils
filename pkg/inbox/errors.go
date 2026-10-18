package inbox

import "errors"

var (
	// ErrInvalidMessage is returned by Store.Insert for a Message with an
	// empty Source, Key or Type.
	ErrInvalidMessage = errors.New("inbox: invalid message")

	// ErrInvalidConfig is returned by NewStore without a pool and by
	// NewWorker without a Store, Source or Handle.
	ErrInvalidConfig = errors.New("inbox: invalid config")

	// ErrRunning is returned by Run and Drain while another Run or Drain of
	// the same Worker is active.
	ErrRunning = errors.New("inbox: worker already running")

	// ErrNotFound is returned by Store.Redrive and Store.Discard for an
	// unknown message ID.
	ErrNotFound = errors.New("inbox: message not found")

	// ErrNotParked is returned by Store.Redrive for a message that is not
	// Parked.
	ErrNotParked = errors.New("inbox: message not parked")

	// ErrSettled is returned by Store.Discard for a message that is already
	// done, ignored or discarded.
	ErrSettled = errors.New("inbox: message already settled")
)
