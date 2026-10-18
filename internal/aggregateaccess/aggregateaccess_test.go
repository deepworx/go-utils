package aggregateaccess_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/aggregateaccess"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
)

type counterID uuid.UUID

type incremented struct{}

func (incremented) EventType() string { return "counter.incremented" }

type counter struct {
	aggregate.Base[counterID]
	n int
}

func (*counter) StreamType() string { return "counter" }

func (c *counter) Apply(aggregate.Event) { c.n++ }

func TestSaveCycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		replayed    int
		raised      int
		wantVersion int
	}{
		{name: "new aggregate", replayed: 0, raised: 2, wantVersion: 2},
		{name: "loaded aggregate", replayed: 3, raised: 1, wantVersion: 4},
		{name: "loaded without command", replayed: 2, raised: 0, wantVersion: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			id := uuid.New()
			c := new(counter)
			aggregateaccess.Init(c, counterID(id))
			for range tt.replayed {
				aggregateaccess.Replay(c, incremented{})
			}
			for range tt.raised {
				aggregate.Raise(c, incremented{})
			}

			if got := aggregateaccess.UUID(c); got != id {
				t.Errorf("UUID = %v, want %v", got, id)
			}
			if c.Version() != tt.wantVersion || c.n != tt.wantVersion {
				t.Errorf("Version, n = %d, %d, want %d", c.Version(), c.n, tt.wantVersion)
			}
			want := slices.Repeat([]aggregate.Event{incremented{}}, tt.raised)
			if tt.raised == 0 {
				want = nil
			}
			if got := aggregateaccess.Uncommitted(c); !slices.Equal(got, want) {
				t.Errorf("Uncommitted = %v, want %v", got, want)
			}

			aggregateaccess.MarkCommitted(c)

			if got := aggregateaccess.Uncommitted(c); got != nil {
				t.Errorf("Uncommitted after MarkCommitted = %v, want nil", got)
			}
			if c.Version() != tt.wantVersion {
				t.Errorf("Version after MarkCommitted = %d, want %d", c.Version(), tt.wantVersion)
			}
		})
	}
}
