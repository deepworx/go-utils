package consumer

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidConfig reports a Config or Option that New rejects.
	ErrInvalidConfig = errors.New("consumer: invalid config")
	// ErrDuplicate reports a second Consumer with the same name on one
	// Runtime.
	ErrDuplicate = errors.New("consumer: duplicate name")
	// ErrRunning reports Run on a Consumer that already runs, or Drain while
	// the Consumer runs or drains.
	ErrRunning = errors.New("consumer: already running")
	// ErrStopped reports a Consumer that is Stopped. It stays Stopped at its
	// Cursor until Runtime.Resume clears it.
	ErrStopped = errors.New("consumer: stopped")
	// ErrNotFound reports a Consumer name the Runtime does not know.
	ErrNotFound = errors.New("consumer: not found")
)

// Permanent marks err as hopeless: the Consumer goes Stopped at once instead
// of retrying. Permanent(nil) returns nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

type permanentError struct{ err error }

func (e *permanentError) Error() string { return fmt.Sprintf("permanent: %v", e.err) }

func (e *permanentError) Unwrap() error { return e.err }

func isPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}
