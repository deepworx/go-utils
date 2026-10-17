package consumer

import (
	"fmt"
	"slices"
)

// filterChange compares a Consumer's configured filter with the one its
// Cursor was last claimed with (ADR-0006). An empty filter means every type.
type filterChange struct {
	stored []string
	// added are the configured types missing from stored.
	added []string
	// widened reports a filtered Cursor claimed unfiltered: every type
	// outside stored is added.
	widened bool
}

func diffFilter(stored, configured []string) filterChange {
	f := filterChange{stored: stored}
	switch {
	case len(stored) == 0:
	case len(configured) == 0:
		f.widened = true
	default:
		for _, t := range configured {
			if !slices.Contains(stored, t) {
				f.added = append(f.added, t)
			}
		}
	}
	return f
}

// needsHistoryCheck reports whether the change is accepted only when no
// event of an added type lies at or below the Cursor. Removed types are
// always accepted, and OwnTransaction applies added types from the Cursor
// onwards.
func (f filterChange) needsHistoryCheck(mode Mode) bool {
	return mode == Transactional && (f.widened || len(f.added) > 0)
}

// cause is the Stop cause of a rejected change.
func (f filterChange) cause() string {
	added := fmt.Sprintf("event types %v", f.added)
	if f.widened {
		added = fmt.Sprintf("every event type besides %v", f.stored)
	}
	return fmt.Sprintf("filter adds %s with events at or below the Cursor: a new Projection version is needed", added)
}
