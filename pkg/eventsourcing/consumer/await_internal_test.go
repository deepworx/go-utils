package consumer

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// pollCounter is a port whose Cursor never reaches pending, counting the
// loads and the most loads in flight at once.
type pollCounter struct {
	port
	pending  eventstore.Cursor
	loads    atomic.Int32
	inFlight atomic.Int32
	peak     atomic.Int32
}

func (p *pollCounter) load(context.Context) (cursorState, error) {
	p.loads.Add(1)
	n := p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	for {
		peak := p.peak.Load()
		if n <= peak || p.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	return cursorState{}, nil
}

func (p *pollCounter) frontier(context.Context, eventstore.Cursor) (frontier, error) {
	return frontier{xmin: p.pending.TransactionID + 1, next: p.pending}, nil
}

func TestAwaitSharesOnePoller(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		awaits  int
		budget  time.Duration
		maxPoll int32
	}{
		{name: "one", awaits: 1, budget: time.Second, maxPoll: 51},
		{name: "many", awaits: 100, budget: time.Second, maxPoll: 51},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				pending := eventstore.Cursor{TransactionID: 7, Position: 7}
				counter := &pollCounter{pending: pending}
				c := &Consumer{awaits: newPoller(counter, slog.New(slog.DiscardHandler))}

				ctx, cancel := context.WithTimeout(context.Background(), tt.budget)
				defer cancel()
				var wg sync.WaitGroup
				for range tt.awaits {
					wg.Go(func() {
						if got := c.Await(ctx, pending); got != Stale {
							t.Errorf("Await = %v, want Stale", got)
						}
					})
				}
				wg.Wait()

				if got := counter.loads.Load(); got > tt.maxPoll {
					t.Errorf("loads = %d for %d Awaits over %s, want at most %d", got, tt.awaits, tt.budget, tt.maxPoll)
				}
				if got := counter.peak.Load(); got != 1 {
					t.Errorf("concurrent loads = %d, want 1", got)
				}
			})
		})
	}
}
