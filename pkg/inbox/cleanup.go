package inbox

import (
	"context"
	"time"

	"github.com/deepworx/go-utils/pkg/scheduler"
)

const (
	// CleanupJobName is the scheduler job name of CleanupJob.
	CleanupJobName = "inbox.cleanup"
	// DefaultRetention is how long CleanupJob keeps settled messages when
	// its retention is not positive.
	DefaultRetention = 7 * 24 * time.Hour
)

// CleanupJob returns a daily scheduler.Job that runs store.Cleanup for the
// messages settled longer than retention ago, measured on the Store clock.
// A retention that is not positive uses DefaultRetention. Parked messages
// are never deleted. The run's Outcome counts the deleted messages as
// Candidates.
func CleanupJob(store *Store, retention time.Duration) scheduler.Job {
	if retention <= 0 {
		retention = DefaultRetention
	}
	return scheduler.Job{
		Name: CleanupJobName,
		Run: func(ctx context.Context, _ scheduler.Run) (scheduler.Outcome, error) {
			deleted, err := store.Cleanup(ctx, store.now().Add(-retention))
			if err != nil {
				return scheduler.Outcome{}, err
			}
			return scheduler.Outcome{Candidates: int(deleted)}, nil
		},
	}
}
