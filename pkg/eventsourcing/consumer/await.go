package consumer

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/deepworx/go-utils/internal/telemetry"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// Freshness tells whether a Consumer has caught up to a Cursor.
type Freshness int

const (
	// Fresh means the Consumer has processed every matching event up to the
	// Cursor.
	Fresh Freshness = iota + 1
	// Stale means the Consumer did not catch up within the budget.
	Stale
	// StaleStopped means the Consumer has not caught up and is Stopped, so
	// it will not catch up without Resume.
	StaleStopped
)

// String returns the name of f.
func (f Freshness) String() string {
	switch f {
	case Fresh:
		return "Fresh"
	case Stale:
		return "Stale"
	case StaleStopped:
		return "StaleStopped"
	}
	return "Freshness(" + strconv.Itoa(int(f)) + ")"
}

const (
	// awaitBudget bounds Await when ctx has no deadline.
	awaitBudget = 2 * time.Second
	// awaitPoll is how often the shared poller checks the Cursor.
	awaitPoll = 20 * time.Millisecond
)

// Await waits until the Consumer has caught up to pos, typically a
// command's CommandResult.Position, so an API can serve read-your-writes
// or mark its response stale. The budget is the ctx deadline, else 2s.
//
// The Consumer has caught up when its Cursor is at or after pos, or when no
// matching event lies between its Cursor and pos and every transaction up
// to pos has ended. So a filtered Consumer whose Cursor sits at its last
// matching event is Fresh. A zero pos is Fresh at once. Await returns
// StaleStopped as soon as it finds the Consumer Stopped short of pos, and
// Stale when the budget runs out. All Awaits on a Consumer share one poller.
func (c *Consumer) Await(ctx context.Context, pos eventstore.Cursor) Freshness {
	if pos.IsZero() {
		return Fresh
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, awaitBudget)
		defer cancel()
	}
	w := c.awaits.add(pos)
	defer c.awaits.remove(w)
	select {
	case f := <-w.done:
		return f
	case <-ctx.Done():
		return Stale
	}
}

// poller checks the Cursor for every pending Await of one Consumer, so
// concurrent Awaits cost one round of queries per poll. Its loop runs while
// any Await waits.
type poller struct {
	port     port
	interval time.Duration
	logger   *slog.Logger

	mu      sync.Mutex
	waiters map[*waiter]struct{}
	// loop is the running poll loop; nil while none runs.
	loop *pollLoop
}

type pollLoop struct {
	stop context.CancelFunc
	done chan struct{}
}

type waiter struct {
	pos  eventstore.Cursor
	done chan Freshness
}

func newPoller(p port, logger *slog.Logger) *poller {
	return &poller{port: p, interval: awaitPoll, logger: logger, waiters: make(map[*waiter]struct{})}
}

// add registers an Await for pos and starts the poll loop if none runs.
func (p *poller) add(pos eventstore.Cursor) *waiter {
	w := &waiter{pos: pos, done: make(chan Freshness, 1)}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.waiters[w] = struct{}{}
	if p.loop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		p.loop = &pollLoop{stop: cancel, done: make(chan struct{})}
		go p.run(ctx, p.loop.done)
	}
	return w
}

// remove drops w. The last Await to leave ends the poll loop and waits for
// it, so no poll outlives the Awaits.
func (p *poller) remove(w *waiter) {
	p.mu.Lock()
	delete(p.waiters, w)
	loop := p.loop
	if len(p.waiters) > 0 || loop == nil {
		p.mu.Unlock()
		return
	}
	p.loop = nil
	p.mu.Unlock()
	loop.stop()
	<-loop.done
}

// resolve hands f to w and stops polling for it. A poll of an ending loop
// may resolve w a second time; that result is dropped.
func (p *poller) resolve(w *waiter, f Freshness) {
	select {
	case w.done <- f:
	default:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.waiters, w)
}

func (p *poller) pending() []*waiter {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Collect(maps.Keys(p.waiters))
}

func (p *poller) run(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		if err := p.poll(ctx); err != nil && ctx.Err() == nil {
			p.logger.DebugContext(ctx, "poll consumer cursor for await", telemetry.LogError, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// poll resolves every pending Await that has caught up, or that cannot
// because the Consumer is Stopped.
func (p *poller) poll(ctx context.Context) error {
	waiters := p.pending()
	if len(waiters) == 0 {
		return nil
	}
	state, err := p.port.load(ctx)
	if err != nil {
		return fmt.Errorf("load cursor: %w", err)
	}
	var ahead *frontier
	for _, w := range waiters {
		if state.cursor.Compare(w.pos) >= 0 {
			p.resolve(w, Fresh)
			continue
		}
		if ahead == nil {
			f, err := p.port.frontier(ctx, state.cursor)
			if err != nil {
				return fmt.Errorf("read frontier: %w", err)
			}
			ahead = &f
		}
		switch {
		case ahead.covers(w.pos):
			p.resolve(w, Fresh)
		case state.stopped != nil:
			p.resolve(w, StaleStopped)
		}
	}
	return nil
}
