# Command transactions run at READ COMMITTED

A Postgres Command transaction serializes writers to one Stream with `pg_advisory_xact_lock`. Under REPEATABLE READ or SERIALIZABLE, the transaction's snapshot is taken when its first statement starts. That statement is the lock itself, or `SET LOCAL app.tenant_id` before it, so it happens before the lock wait ends. Everything read after the wait comes from that snapshot and misses what the previous lock holder committed: the Stream version, the Aggregate's events, and guard reads. A second writer then passes the Expected version check on a stale version, and only `events_stream_position_key` stops it from appending a duplicate `stream_position`. Under SERIALIZABLE the conflict surfaces as a raw serialization failure (`40001`) instead of `ErrConcurrencyConflict`.

READ COMMITTED takes a new snapshot for every statement, so every read after the lock sees the previous holder's commit. `PgUnitOfWork.Execute` therefore reads `transaction_isolation` in the same statement that takes the lock, and fails with `ErrUnsupportedIsolation` for any other level. That covers transactions it opens and outer transactions it joins.

## Considered Options

- **Document the restriction only.** This was rejected because the failure is silent: commands keep working and only fail under contention, with an error that names a constraint instead of the cause.
- **Reject non-READ COMMITTED `TxOptions` in `NewPgUnitOfWork`.** This was rejected because `Execute` joins outer transactions from ctx, whose options the constructor never sees, and because it would need an accessor for the options on `postgres.PgUnitOfWork`.
- **Support SERIALIZABLE.** This was rejected because the stream lock already serializes the writers it protects, and the extra guarantees would cost every command a retry loop for serialization failures.

## Consequences

- The check costs no extra round trip. A misconfigured transaction waits for the stream lock before it is rejected, since the lock and the check are one statement.
- READ UNCOMMITTED, which Postgres runs as READ COMMITTED, is rejected too: the check compares the configured level.
- The stream lock serializes only writers to this Stream. Reads of other data inside `fn`, such as a guard on a key shared across Streams, are as fresh as READ COMMITTED and no more; their correctness rests on their own locks or constraints.
- `events_stream_position_key` stays mandatory (ADR-0003). It still catches writers that bypass the stream lock.
