package reactor

import "errors"

var (
	// ErrStartAtRequired reports a Config without StartAt: where a new
	// Reactor begins must be chosen explicitly.
	ErrStartAtRequired = errors.New("reactor: StartAt is required")
	// ErrNotParked reports a Stream or event the Reactor does not hold.
	ErrNotParked = errors.New("reactor: not parked")
)
