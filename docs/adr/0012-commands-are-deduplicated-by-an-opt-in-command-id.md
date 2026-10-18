# Commands are deduplicated by an opt-in Command ID

Clients that buffer commands offline, such as a mobile app, resend a command when its response was lost. Aggregate state guards cover idempotent commands (`Complete` on a completed task), but not ones like `+1` or `Toggle`. A client may therefore pass a **Command ID** to `Repository.Create` and `Repository.Update` with `eventstore.WithCommandID`. It makes the command take effect at most once per Tenant.

- The Command ID is recorded in `processed_commands` under `(tenant_id, command_id)` in the Command transaction, together with the Stream and the `CommandResult`. It is recorded also when the command raises nothing, so a repeat returns the same Version. A rejected command rolls back and leaves no record, so a retry runs again.
- A repeat returns the original `CommandResult` with `Replayed` set and does not run the command. A repeated `Create` returns that result instead of `ErrAlreadyExists`.
- The lookup runs before the Expected version check, so the retry of a version-guarded command does not conflict with its own first attempt.
- The same Command ID on another Stream fails with `ErrCommandIDReused`. When two commands on different Streams record the same Command ID concurrently, the later one fails with `ErrConcurrencyConflict`; its retry gets the definite answer.
- Commands without a Command ID behave as before.

## Boundary with ADR-0007

ADR-0007 still holds for everything the event log drives: Reactors stay at-least-once, the commands they issue rely on aggregate state guards, and outbound calls use `Envelope.EventID` as their idempotency key. The Command ID only covers commands that enter from outside the event log through `Create` and `Update`. `Save` and Reactors do not take one.

## Considered Options

- **Aggregate state guards only.** This was rejected because non-idempotent commands have no state to guard on, and pushing a list of seen IDs into every Aggregate bloats its state and its Stream.
- **Dedupe by request ID.** This was rejected because a request ID names one HTTP request, and an offline client resends the command in a new request. It is also reused as the Correlation (ADR-0010), whose purpose is tracing, not dedupe.
- **A Command ID unique per Stream.** This was rejected because a client that reuses an ID for another Stream has a bug, and per-Tenant uniqueness surfaces it as `ErrCommandIDReused` instead of running the command.
- **Recording the Command ID on the events.** This was rejected because a command that raises nothing has no event to carry it, and the lookup would scan the append-only log (ADR-0009), which is never cleaned up.

## Consequences

- `docs/postgres/schema.sql` gains `processed_commands` with an index on `recorded_at`; `recorded_at` comes from the store clock (`WithClock`). The service schedules the cleanup (`CommandCleanupJob`, default retention 30 days), and a repeat after cleanup runs the command again, so the retention bounds how long a client may buffer a command.
- `CommandOption` replaces `UpdateOption`, and `Create` accepts it too. `ExpectVersion` is a `CommandOption`; `Create` ignores it, since it always expects an empty Stream.
- A deduplicated command costs one lookup and one insert in its Command transaction.
- Out of scope: a delta read API (clients read projection snapshots with their Version), a client-supplied event time (it stays in the payload, ADR-0010), and batch or merge helpers.
