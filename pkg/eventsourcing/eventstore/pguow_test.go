package eventstore_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// TestStreamLockKey pins the stream lock formula: changing it breaks mutual
// exclusion between pods that run different versions.
func TestStreamLockKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		schema string
		tenant string
		stream string
		want   int64
	}{
		{"nil stream", "public", "t1", "00000000-0000-0000-0000-000000000000", -4810183150112141353},
		{"other schema", "alt", "t1", "6ba7b810-9dad-11d1-80b4-00c04fd430c8", 5069779516414534771},
		{"other tenant", "public", "t2", "6ba7b810-9dad-11d1-80b4-00c04fd430c8", -2999198086579875674},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := eventstore.StreamLockKey(tt.schema, tt.tenant, uuid.MustParse(tt.stream))
			if got != tt.want {
				t.Fatalf("StreamLockKey(%q, %q, %s) = %d, want %d", tt.schema, tt.tenant, tt.stream, got, tt.want)
			}
		})
	}
}
