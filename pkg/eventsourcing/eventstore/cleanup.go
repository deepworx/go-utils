package eventstore

import (
	"context"
	"time"

	"github.com/deepworx/go-utils/pkg/scheduler"
)

const (
	// CommandCleanupJobName is the scheduler job name of CommandCleanupJob.
	CommandCleanupJobName = "eventstore.command_cleanup"
	// DefaultCommandRetention is how long CommandCleanupJob keeps Command
	// IDs when its retention is not positive.
	DefaultCommandRetention = 30 * 24 * time.Hour
)

// CommandCleanupJob returns a daily scheduler.Job that runs
// store.CleanupCommands for the Command IDs recorded longer than retention
// ago, measured on the store clock. A retention that is not positive uses
// DefaultCommandRetention (30 days). A repeat of a deleted Command ID runs
// as a new command, so a client must not buffer and resend a command for
// longer than retention. The
// run's Outcome counts the deleted Command IDs as Candidates.
func CommandCleanupJob(store Store, retention time.Duration) scheduler.Job {
	if retention <= 0 {
		retention = DefaultCommandRetention
	}
	return scheduler.Job{
		Name: CommandCleanupJobName,
		Run: func(ctx context.Context, _ scheduler.Run) (scheduler.Outcome, error) {
			deleted, err := store.CleanupCommands(ctx, store.now().Add(-retention))
			if err != nil {
				return scheduler.Outcome{}, err
			}
			return scheduler.Outcome{Candidates: int(deleted)}, nil
		},
	}
}
