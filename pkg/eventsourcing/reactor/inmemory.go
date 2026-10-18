package reactor

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// memParks is the park list of a Reactor on an InMemoryRuntime. Each hold
// has its own mutex, the in-memory counterpart of the Pg row lock; a
// redrive claims the head instead of keeping the mutex while it reacts.
type memParks struct {
	mu    sync.Mutex
	holds map[Stream]*memHold
	// order is the order Streams were Parked in.
	order uint64
	// discarded is the audit trail of Discard, in discard order.
	discarded []memDiscarded
}

// memDiscarded is a discarded event, kept like a Pg row with discarded_at
// and discarded_by.
type memDiscarded struct {
	Parked
	by string
	at time.Time
}

type memHold struct {
	mu     sync.Mutex
	seq    uint64
	events []memParked
	// redrives counts the failed auto-redrives; next is the next one, zero
	// for none.
	redrives  int
	next      time.Time
	exhausted bool
	// claim is the token of the redrive replaying the head, valid until
	// claimedUntil.
	claim        uuid.UUID
	claimedUntil time.Time
}

func (h *memHold) claimed(now time.Time) bool {
	return h.claim != uuid.Nil && h.claimedUntil.After(now)
}

func (h *memHold) due(now time.Time) bool {
	return !h.exhausted && !h.next.IsZero() && !h.next.After(now)
}

type memParked struct {
	Parked
	env     eventstore.Envelope
	payload *eventstore.Payload
}

func newMemParks() *memParks {
	return &memParks{holds: make(map[Stream]*memHold)}
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
	if len(h.events) == 0 {
		h.next = in.nextAttempt
	}
	h.add(in)
	return nil
}

func (m *memParks) claimHead(_ context.Context, in claimInput) (claimedHead, error) {
	h, ok := m.lock(in.stream, false)
	if !ok {
		return claimedHead{}, ErrNotParked
	}
	defer h.mu.Unlock()
	switch {
	case in.auto != nil && !h.due(in.auto.now):
		return claimedHead{}, ErrNotParked
	case h.claimed(in.now):
		return claimedHead{}, ErrRedriving
	case len(h.events) == 0:
		m.release(in.stream)
		return claimedHead{empty: true}, nil
	}
	h.claim, h.claimedUntil = in.token, in.until
	head := h.events[0]
	return claimedHead{env: head.env, payload: head.payload}, nil
}

func (m *memParks) settleHead(_ context.Context, in settleInput) (int, error) {
	h, ok := m.lock(in.stream, false)
	if !ok {
		return 0, nil
	}
	defer h.mu.Unlock()
	if h.claim != in.token {
		return 0, fmt.Errorf("claim on %s/%s lost: %w", in.stream.Tenant, in.stream.ID, ErrRedriving)
	}
	h.claim, h.claimedUntil = uuid.Nil, time.Time{}
	i := slices.IndexFunc(h.events, func(e memParked) bool { return e.EventID == in.eventID })
	switch {
	case in.failure == nil && i >= 0:
		h.events = slices.Delete(h.events, i, i+1)
	case in.failure != nil:
		if i >= 0 {
			h.events[i].Cause = in.failure.Error()
			h.events[i].Attempts++
		}
		if in.auto != nil {
			h.redrives++
			h.next, h.exhausted = in.auto.next(h.redrives)
		}
	}
	if len(h.events) == 0 {
		m.release(in.stream)
	}
	return len(h.events), nil
}

func (m *memParks) discard(_ context.Context, in discardInput) error {
	h, ok := m.lock(in.stream, false)
	if !ok {
		return ErrNotParked
	}
	defer h.mu.Unlock()
	if h.claimed(in.at) {
		return ErrRedriving
	}
	i := slices.IndexFunc(h.events, func(e memParked) bool { return e.EventID == in.eventID })
	if i < 0 {
		return ErrNotParked
	}
	gone := h.events[i]
	h.events = slices.Delete(h.events, i, i+1)
	m.mu.Lock()
	m.discarded = append(m.discarded, memDiscarded{Parked: gone.Parked, by: in.by, at: in.at})
	if len(h.events) == 0 {
		delete(m.holds, in.stream)
	}
	m.mu.Unlock()
	return nil
}

// snapshot returns the holds of tenant, or of all for "". Their contents
// must be read under each hold's lock.
func (m *memParks) snapshot(tenant string) map[Stream]*memHold {
	m.mu.Lock()
	defer m.mu.Unlock()
	holds := make(map[Stream]*memHold, len(m.holds))
	for s, h := range m.holds {
		if tenant == "" || s.Tenant == tenant {
			holds[s] = h
		}
	}
	return holds
}

func (m *memParks) due(_ context.Context, now time.Time) ([]Stream, error) {
	next := make(map[Stream]time.Time)
	for s, h := range m.snapshot("") {
		h.mu.Lock()
		if h.due(now) {
			next[s] = h.next
		}
		h.mu.Unlock()
	}
	return slices.SortedFunc(maps.Keys(next), func(a, b Stream) int {
		return next[a].Compare(next[b])
	}), nil
}

func (m *memParks) nextDue(context.Context) (time.Time, bool, error) {
	var earliest time.Time
	for _, h := range m.snapshot("") {
		h.mu.Lock()
		if !h.exhausted && !h.next.IsZero() && (earliest.IsZero() || h.next.Before(earliest)) {
			earliest = h.next
		}
		h.mu.Unlock()
	}
	return earliest, !earliest.IsZero(), nil
}

func (m *memParks) stats(context.Context) (parkStats, error) {
	var st parkStats
	for _, h := range m.snapshot("") {
		h.mu.Lock()
		if len(h.events) > 0 {
			st.streams++
			st.events += len(h.events)
			if h.exhausted {
				st.exhausted++
			}
		}
		h.mu.Unlock()
	}
	return st, nil
}

func (m *memParks) list(_ context.Context, tenant string) ([]Parked, error) {
	holds := m.snapshot(tenant)
	streams := slices.SortedFunc(maps.Keys(holds), func(a, b Stream) int {
		return cmp.Compare(holds[a].seq, holds[b].seq)
	})
	var out []Parked
	for _, s := range streams {
		h := holds[s]
		h.mu.Lock()
		for _, e := range h.events {
			p := e.Parked
			p.NextAttemptAt, p.Exhausted = h.next, h.exhausted
			out = append(out, p)
		}
		h.mu.Unlock()
	}
	return out, nil
}
