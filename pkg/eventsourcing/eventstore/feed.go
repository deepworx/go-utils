package eventstore

import (
	"math"
	"slices"

	"github.com/deepworx/go-utils/internal/storehook"
)

func init() {
	storehook.RegisterInMemoryFeed(func(store any) (any, bool) {
		s, ok := store.(*InMemoryStore)
		if !ok || s == nil {
			return nil, false
		}
		return memFeed{codec: s.codec, log: s.log}, true
	})
	storehook.RegisterPgStore(func(store any) (any, bool) {
		s, ok := store.(*PgStore)
		if !ok || s == nil {
			return nil, false
		}
		return pgSource{codec: s.codec, schema: s.schema}, true
	})
}

// pgSource is what the Pg consumer runtime needs from a PgStore, reached
// through internal/storehook. The runtime queries the events table itself.
type pgSource struct {
	codec  *Codec
	schema string
}

// Codec returns the Codec the store encodes with.
func (s pgSource) Codec() *Codec { return s.codec }

// Schema returns the schema that qualifies the events table.
func (s pgSource) Schema() string { return s.schema }

// memFeed is the read side of an InMemoryStore for the in-memory consumer
// runtime, reached through internal/storehook. Like Postgres below
// pg_snapshot_xmin, it shows only events whose transaction ID lies below
// every open Command transaction, so a Consumer never skips an event that
// commits later with a smaller Cursor.
type memFeed struct {
	codec *Codec
	log   *memLog
}

// Codec returns the Codec the store encodes with.
func (f memFeed) Codec() *Codec { return f.codec }

// Changed returns a channel that is closed once more events may be
// visible.
func (f memFeed) Changed() <-chan struct{} {
	f.log.mu.Lock()
	defer f.log.mu.Unlock()
	return f.log.changed
}

// Next returns the first visible event after the Cursor after whose Event
// type is in types, or any type when types is empty.
func (f memFeed) Next(after Cursor, types []string) (Envelope, Payload, bool) {
	l := f.log
	l.mu.Lock()
	defer l.mu.Unlock()
	horizon := l.horizon()
	var (
		next  memRecord
		found bool
	)
	for _, records := range l.ranges(types) {
		i, exact := slices.BinarySearchFunc(records, after, compareRecord)
		if exact {
			i++
		}
		if i == len(records) || records[i].env.Cursor.TransactionID >= horizon {
			continue
		}
		if !found || records[i].env.Cursor.Compare(next.env.Cursor) < 0 {
			next, found = records[i], true
		}
	}
	return next.env, next.payload, found
}

// Head returns the Cursor of the last visible event whose Event type is in
// types, or any type when types is empty. It is zero when there is none.
func (f memFeed) Head(types []string) Cursor {
	l := f.log
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := Cursor{TransactionID: l.horizon()}
	var head Cursor
	for _, records := range l.ranges(types) {
		i, _ := slices.BinarySearchFunc(records, limit, compareRecord)
		if i > 0 && records[i-1].env.Cursor.Compare(head) > 0 {
			head = records[i-1].env.Cursor
		}
	}
	return head
}

// Horizon returns the transaction ID below which every Command transaction
// has ended, like pg_snapshot_xmin.
func (f memFeed) Horizon() uint64 {
	f.log.mu.Lock()
	defer f.log.mu.Unlock()
	return f.log.horizon()
}

// ranges returns the Cursor-ordered record lists to read for types. The
// caller holds l.mu.
func (l *memLog) ranges(types []string) [][]memRecord {
	if len(types) == 0 {
		return [][]memRecord{l.all}
	}
	ranges := make([][]memRecord, 0, len(types))
	for _, t := range types {
		ranges = append(ranges, l.byType[t])
	}
	return ranges
}

// horizon returns the smallest transaction ID of an open Command
// transaction that has appended; every committed event below it is
// visible. The caller holds l.mu.
func (l *memLog) horizon() uint64 {
	horizon := uint64(math.MaxUint64)
	for _, p := range l.pending {
		horizon = min(horizon, p.xid)
	}
	return horizon
}
