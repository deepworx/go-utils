// Package hook lets package aggregate hand its bookkeeping to
// internal/aggregateaccess without exporting it.
//
// aggregate registers a lookup at init time. aggregateaccess imports
// aggregate, so the lookup is always registered before it is used. Like
// aggregate itself, this package imports only the stdlib and uuid.
package hook

import "github.com/google/uuid"

// Event mirrors aggregate.Event; both interfaces are mutually assignable.
type Event interface {
	EventType() string
}

// State is the bookkeeping that aggregate.Base embeds.
type State struct {
	ID          uuid.UUID
	Version     int
	Uncommitted []Event
}

var stateOf func(r any) *State

// Register installs the lookup from an aggregate Root to its State. Only
// package aggregate calls it.
func Register(lookup func(r any) *State) {
	stateOf = lookup
}

// StateOf returns the State embedded in the aggregate Root r.
func StateOf(r any) *State {
	return stateOf(r)
}
