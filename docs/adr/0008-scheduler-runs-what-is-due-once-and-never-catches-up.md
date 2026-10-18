# The scheduler runs what is due once and never catches up

`pkg/scheduler` runs two kinds of jobs: **daily** jobs on a Business date in one required business `Location` (optionally delayed by `NotBefore`), and **interval** jobs on a fixed grid of multiples of `Every` on Unix time. Both persist their state in Postgres: daily jobs keep one row per `(job, business_date)` in `scheduler_runs`, interval jobs keep one row per job with its next `due_at` in `scheduler_intervals`. After downtime a job runs **once**: missed Business dates get no row, and an interval job's `due_at` jumps to the next grid point after now. What is due is derived by the job from state, so every job must be idempotent for the same date or grid point.

## Considered Options

- **One run per missed date or slot.** This was rejected because a state-derived sweep finds the same candidates on every replayed date; catching up a week would run the same sweep seven times.
- **Cron expressions.** This was rejected because they need a parser or a dependency, and a "monthly" job is a daily sweep whose query finds nothing on most days.
- **Interval state in the leader's memory.** This was rejected because every rollout or failover resets the clock and runs the job early; with frequent releases the cadence drifts arbitrarily.
- **Fixed delay (`finished_at + Every`).** This was rejected because each run and each outage shifts the cadence; the grid keeps it stable across rollouts.

## Consequences

- An aborted run keeps its row open and is retried after `RetryAfter` (for interval jobs `min(RetryAfter, Every)`); a finished date or grid point is never run again.
- `scheduler_runs` grows by one row per daily job per day and the library never prunes it; `scheduler_intervals` stays one row per job.
- The `scheduler.job.last_success` gauge reads from the tables, so a new leader reports the truth for both kinds.
- `Tick(ctx, now)` is exported, so daylight-saving cases are tested with explicit instants rather than months of synctest time.
