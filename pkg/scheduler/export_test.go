package scheduler

import (
	"context"

	"github.com/deepworx/go-utils/pkg/postgres/lease"
)

// WithoutLeases makes every lease acquisition succeed, so tests can reach
// the claim fence with two schedulers running the same job at once.
func WithoutLeases() Option {
	return func(o *options) { o.locker = freeLocker{} }
}

type freeLocker struct{}

func (freeLocker) tryAcquire(ctx context.Context, _ lease.Key) (context.Context, func(), error) {
	return ctx, func() {}, nil
}
