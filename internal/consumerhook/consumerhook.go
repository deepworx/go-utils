// Package consumerhook lets package consumer hand a Reactor and a
// Projection what they need from a Runtime and its runner without
// exporting it.
//
// consumer registers its hooks at init time. reactor and projection import
// consumer, so the hooks are always registered before they are used.
package consumerhook

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

// Backend is the event log behind a consumer Runtime.
type Backend struct {
	Codec *eventstore.Codec
	// Pool and Schema are set for a PgRuntime; Pool is nil in memory.
	Pool   *pgxpool.Pool
	Schema string
}

// DecodeFailedFunc receives an event whose payload does not decode, in
// place of the Consumer going Stopped. ctx carries the event's Tenant and
// Correlation. A returned error is retried like a handler error.
type DecodeFailedFunc func(ctx context.Context, env eventstore.Envelope, payload eventstore.Payload, err error) error

// ReactorOptions are the runner behaviours only a Reactor turns on.
type ReactorOptions struct {
	// SeedAtHead seeds a new Cursor at the head of the log when the
	// Consumer first claims it, so history is skipped.
	SeedAtHead bool
	// DecodeFailed, when set, receives undecodable events.
	DecodeFailed DecodeFailedFunc
}

// Changes sends and delivers the change payloads of one Runtime, for
// package projection.
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

// Hooks are the lookups package consumer registers.
type Hooks struct {
	// Changes returns the change channel of a consumer Runtime, or false
	// when rt is none.
	Changes func(rt any) (Changes, bool)
	// Reset takes the leases of in.Consumers and runs in.Clear and the
	// rewind of their Cursors to the start of the log in one Cursor
	// transaction. It fails with consumer.ErrRunning while one of them runs.
	Reset func(ctx context.Context, in ResetInput) error
	// Backend returns the Backend of a consumer Runtime, or false when rt is
	// none.
	Backend func(rt any) (Backend, bool)
	// ReactorOption returns a consumer.Option applying o.
	ReactorOption func(o ReactorOptions) any
	// IsPermanent reports whether err is marked consumer.Permanent.
	IsPermanent func(err error) bool
}

var hooks Hooks

// Register installs the hooks. Only package consumer calls it.
func Register(h Hooks) { hooks = h }

// RuntimeBackend returns the Backend of a consumer Runtime, or false when rt
// is none.
func RuntimeBackend(rt any) (Backend, bool) {
	if hooks.Backend == nil {
		return Backend{}, false
	}
	return hooks.Backend(rt)
}

// ReactorOption returns a consumer.Option applying o; the caller asserts it.
func ReactorOption(o ReactorOptions) any { return hooks.ReactorOption(o) }

// IsPermanent reports whether err is marked consumer.Permanent.
func IsPermanent(err error) bool { return hooks.IsPermanent(err) }

// RuntimeChanges returns the change channel of a consumer Runtime, or false
// when rt is none.
func RuntimeChanges(rt any) (Changes, bool) {
	if hooks.Changes == nil {
		return nil, false
	}
	return hooks.Changes(rt)
}

// Reset runs Hooks.Reset.
func Reset(ctx context.Context, in ResetInput) error { return hooks.Reset(ctx, in) }
