// Package projectionhook lets the consumer runtimes hand package projection
// what it needs beyond the public consumer API: the change channel of a
// Runtime and resetting Consumers.
//
// consumer registers its implementations at init time. projection imports
// consumer, so they are always registered before they are used.
package projectionhook

import (
	"context"

	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

// Changes sends and delivers the change payloads of one Runtime.
type Changes interface {
	// Publish sends payload when the transaction in ctx commits, or at once
	// outside one.
	Publish(ctx context.Context, payload string) error
	// Subscribe returns a Feed of every payload published from now on.
	Subscribe() (Feed, error)
}

// Feed delivers change payloads. *notify.Subscription is one.
type Feed interface {
	C() <-chan notify.Notification
	Close()
}

// ResetInput names the Consumers to reset and the read model to clear.
type ResetInput struct {
	// Consumers are *consumer.Consumer values of one Runtime.
	Consumers []any
	// Clear empties the read model inside the reset transaction.
	Clear func(ctx context.Context) error
}

var (
	changes func(rt any) (Changes, bool)
	reset   func(ctx context.Context, in ResetInput) error
)

// Register installs the consumer implementations. Only package consumer
// calls it.
func Register(changesOf func(rt any) (Changes, bool), resetFn func(ctx context.Context, in ResetInput) error) {
	changes, reset = changesOf, resetFn
}

// ChangesOf returns the change channel of a consumer Runtime, or false when
// rt is none.
func ChangesOf(rt any) (Changes, bool) {
	if changes == nil {
		return nil, false
	}
	return changes(rt)
}

// Reset takes the leases of in.Consumers and runs in.Clear and the rewind of
// their Cursors to the start of the log in one Cursor transaction. It fails
// with consumer.ErrRunning while one of them runs.
func Reset(ctx context.Context, in ResetInput) error {
	return reset(ctx, in)
}
