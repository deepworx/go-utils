package consumer

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// Group runs Consumers together.
type Group []*Consumer

// Run runs each Consumer in its own goroutine until ctx ends, then returns
// nil once all have returned. When one fails, the others are cancelled and
// its error is returned.
func (g Group) Run(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)
	for _, c := range g {
		eg.Go(func() error { return c.Run(ctx) })
	}
	return eg.Wait()
}
