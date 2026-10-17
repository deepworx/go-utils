package notify

import "errors"

var (
	// ErrInvalidChannel is returned for a channel name that is empty, longer
	// than 63 bytes or contains a NUL byte.
	ErrInvalidChannel = errors.New("notify: invalid channel name")

	// ErrRunning is returned by Run while another Run of the same Listener is
	// active.
	ErrRunning = errors.New("notify: listener already running")

	// ErrNilPool is returned by NewListener without a pool.
	ErrNilPool = errors.New("notify: pool is required")
)
