package reactor

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// memParks is the park list of a Reactor on an InMemoryRuntime. Each hold
// has its own mutex, the in-memory counterpart of the Pg row lock.
type memParks struct {
	codec *eventstore.Codec

	mu    sync.Mutex
	holds map[Stream]*memHold
	// order is the order Streams were Parked in.
	order uint64
}

type memHold struct {
	mu     sync.Mutex
	seq    uint64
	events []memParked
}

type memParked struct {
	Parked
	env     eventstore.Envelope
	payload *eventstore.Payload
}

func newMemParks(codec *eventstore.Codec) *memParks {
	return &memParks{codec: codec, holds: make(map[Stream]*memHold)}
}

// lock returns the locked hold of s, creating it when create is set. It
// reports false when s has no hold.
func (m *memParks) lock(s Stream, create bool) (*memHold, bool) {
	for {
		m.mu.Lock()
		h := m.holds[s]
		if h == nil && create {
			m.order++
			h = &memHold{seq: m.order}
			m.holds[s] = h
		}
		m.mu.Unlock()
		if h == nil {
			return nil, false
		}
		h.mu.Lock()
		m.mu.Lock()
		current := m.holds[s] == h
		m.mu.Unlock()
		if current {
			return h, true
		}
		h.mu.Unlock()
	}
}

// release deletes the hold of s; the caller holds h's lock.
func (m *memParks) release(s Stream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.holds, s)
}

func (h *memHold) add(in parkInput) {
	for _, e := range h.events {
		if e.EventID == in.env.EventID {
			return
		}
	}
	h.events = append(h.events, memParked{Parked: newParked(in), env: in.env, payload: in.payload})
}

func streamOf(env eventstore.Envelope) Stream {
	return Stream{Tenant: env.TenantID, ID: env.StreamID}
}

func (m *memParks) holdIfParked(_ context.Context, in parkInput) (bool, error) {
	h, ok := m.lock(streamOf(in.env), false)
	if !ok {
		return false, nil
	}
	defer h.mu.Unlock()
	h.add(in)
	return true, nil
}

func (m *memParks) park(_ context.Context, in parkInput) error {
	h, _ := m.lock(streamOf(in.env), true)
	defer h.mu.Unlock()
	h.add(in)
	return nil
}

func (m *memParks) redriveHead(ctx context.Context, in redriveInput) (headResult, error) {
	h, ok := m.lock(in.stream, false)
	if !ok {
		return headResult{}, ErrNotParked
	}
	defer h.mu.Unlock()
	if len(h.events) == 0 {
		m.release(in.stream)
		return headResult{}, nil
	}
	head := &h.events[0]
	if err := m.replay(ctx, in.replay, head); err != nil {
		head.Cause = err.Error()
		head.Attempts++
		return headResult{remaining: len(h.events), failure: err}, nil
	}
	h.events = h.events[1:]
	if len(h.events) == 0 {
		m.release(in.stream)
	}
	return headResult{replayed: true, remaining: len(h.events)}, nil
}

func (m *memParks) replay(ctx context.Context, replay replayFunc, p *memParked) error {
	env := p.env
	if p.payload != nil {
		event, err := m.codec.Decode(*p.payload)
		if err != nil {
			return fmt.Errorf("event %s: %w", env.EventID, err)
		}
		env.Event = event
	}
	return replay(ctx, env)
}

func (m *memParks) discard(_ context.Context, in discardInput) error {
	h, ok := m.lock(in.stream, false)
	if !ok {
		return ErrNotParked
	}
	defer h.mu.Unlock()
	i := slices.IndexFunc(h.events, func(e memParked) bool { return e.EventID == in.eventID })
	if i < 0 {
		return ErrNotParked
	}
	h.events = slices.Delete(h.events, i, i+1)
	if len(h.events) == 0 {
		m.release(in.stream)
	}
	return nil
}

func (m *memParks) list(_ context.Context, tenant string) ([]Parked, error) {
	m.mu.Lock()
	holds := make(map[Stream]*memHold, len(m.holds))
	for s, h := range m.holds {
		if tenant == "" || s.Tenant == tenant {
			holds[s] = h
		}
	}
	m.mu.Unlock()

	streams := slices.SortedFunc(maps.Keys(holds), func(a, b Stream) int {
		return cmp.Compare(holds[a].seq, holds[b].seq)
	})
	var out []Parked
	for _, s := range streams {
		h := holds[s]
		h.mu.Lock()
		for _, e := range h.events {
			out = append(out, e.Parked)
		}
		h.mu.Unlock()
	}
	return out, nil
}
