package inbox

import "errors"

var (
	// ErrInvalidMessage is returned by Store.Insert for a Message with an
	// empty Source, Key or Type.
	ErrInvalidMessage = errors.New("inbox: invalid message")

	// ErrInvalidConfig is returned by NewWorker for a Config without a Source
	// or Handle.
	ErrInvalidConfig = errors.New("inbox: invalid worker config")

	// ErrRunning is returned by Run and Drain while another Run or Drain of
	// the same Worker is active.
	ErrRunning = errors.New("inbox: worker already running")
)
