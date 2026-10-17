# Projection versions are switched by the service's migrations

A Projection's read model is versioned: version N lives in tables `<table>_vN`, is built by Consumer `<name>@vN`, and readers query a view `<table>` that points at the active version. The service's own golang-migrate migrations create every versioned table and switch the view; the library runs no DDL for projections either (extending ADR-0003) and only runs the versioned Consumers inside the Cursor transaction. A version bump spans two releases: release N creates `_vN+1` and runs the new handler as a shadow beside the still-live active version; release N+1 switches the view, guarded by a migration that fails while the shadow lags the log head. Cleanup of `_vN` and its Cursor row follows in a later release.

## Considered Options

- **CPQ's runtime shadow build and swap** (`CREATE TABLE … LIKE … INCLUDING ALL`, rename, recreate view from a CLI). This was rejected because the app role would need DDL rights, and the swap races the live Consumer: an event handled between the shadow's last batch and the swap lands in the table that is dropped, and the live Cursor is already past it.
- **Runtime DDL behind a separately privileged role or CLI.** This was rejected because it keeps the race and adds a second credential and code path for an operation the service's migrations already handle.
- **Freezing the old version during a bump** (CPQ ships only the new renderer). This was rejected because readers would see stale data for a whole release cycle.

## Consequences

- A same-version repair (handler bug, unchanged shape) is a version bump with an identical table. `projection.Reset` (clear tables and Cursor in one transaction) exists only for prototypes and development, where readers may see an empty model.
- Registration names the active and the shadow version explicitly. Only the active version sends change notifications and is the target of `Await`.
- A view switch must be column-compatible (new version ⊇ old columns, same types), because pods of the previous release still read through the view during the rollout. Removing or retyping a column takes a later bump.
- Handlers are plain code with their own sqlc queries against their versioned tables. CPQ's generic table renderer is not ported.
- `Drain` is not part of the rebuild. Catch-up is observed via the lag gauges and the `CaughtUp` method of the Projection.
