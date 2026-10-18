package eventstore

import "github.com/google/uuid"

// ShareLog makes dst read and append to the log of src, so the contract
// suite can open stores with different Codecs over one log.
func ShareLog(dst, src *InMemoryStore) {
	dst.log = src.log
}

// TraceContexts returns the stored trace context of every committed event
// of a Stream in stream order.
func TraceContexts(s *InMemoryStore, tenant string, stream uuid.UUID) []string {
	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	var out []string
	for _, r := range s.log.streams[streamKey{tenant: tenant, stream: stream}] {
		out = append(out, r.traceContext)
	}
	return out
}

// StreamLockKey returns the advisory lock key of a Stream of tenant in
// schema.
func StreamLockKey(schema, tenant string, stream uuid.UUID) int64 {
	return streamRef{schema: schema, tenant: tenant, stream: stream}.lockKey()
}
