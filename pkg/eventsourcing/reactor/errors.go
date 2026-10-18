package reactor

import "errors"

var (
	// ErrStartAtRequired reports a Config without StartAt: where a new
	// Reactor begins must be chosen explicitly.
	ErrStartAtRequired = errors.New("reactor: StartAt is required")
	// ErrNotParked reports a Stream or event the Reactor does not hold.
	ErrNotParked = errors.New("reactor: not parked")
	// ErrRedriving reports a Stream that another Redrive or auto-redrive is
	// replaying right now. Its claim expires after the reaction timeout
	// plus 30s, so a crashed redrive frees the Stream again.
	ErrRedriving = errors.New("reactor: redrive in progress")
)
