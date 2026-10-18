# Leases are not fences: holders fence their own writes

`lease.Claims` grants mutual exclusion through Postgres session advisory locks, all held on one hijacked connection per process. A lease is lost only when that session ends, and the holder learns of it at the next ping (default 5 s), when its derived context is cancelled with cause `lease.ErrLost`. In that window another pod can acquire the same lease while the old holder is still working. A lease therefore only prevents duplicate work; it does not guarantee correctness.

Every lease holder must make its own writes safe against a concurrent former holder:

- **Consumers:** the Cursor update is compare-and-set on the expected position, so a stale holder's Cursor transaction fails and rolls back its effect.
- **Scheduler:** `scheduler_runs` and `scheduler_intervals` are claimed with conditional upserts, so a run executes at most once per date or grid point.
- **Inbox:** rows are fenced by `claim_generation`; the `inbox:telemetry` lease only gates gauges, where a duplicate reading is harmless.

## Considered Options

- **Fencing tokens from a `leases` table.** Each acquire increments a generation that holders pass to their writes. This was rejected because every user already has a natural fence in its own table, and a shared token couples all packages to one more table and one more write per acquire.
- **One connection per lease.** This was rejected because it costs one backend per Consumer and job lock, and still needs a ping per connection to detect loss. A single connection loses all leases at once, which holders must handle anyway.

## Consequences

- New lease users must name their fence in their design; "we hold the lease" is never sufficient on its own.
- Session locks use the two-int advisory space (`fnv32a(namespace)`, `fnv32a(name)`); the transaction-scoped bigint space belongs to stream locks. Host apps must not take session locks in classes derived from the library's namespaces.
- PgBouncer in transaction mode is not supported, because session locks and `LISTEN` need a real session.
