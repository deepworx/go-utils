# Event metadata: actor, correlation and causation

Every event row carries `actor TEXT NOT NULL`, `correlation_id TEXT NOT NULL`, `causation_id UUID NULL` and `recorded_at TIMESTAMPTZ NOT NULL`, all filled by the store from the context at append time; callers never pass them per event.

- **Actor** resolves from `ctxutil.WithActor`, then `Claims.UserID`; if both are missing or empty, the append fails with `eventstore.ErrNoActor`. A user is stored as their raw user ID. Library components stamp `system:<component>:<name>` (`system:reactor:<name>`, `system:scheduler:<job>`, `system:inbox:<source>`); the `system:` prefix is reserved.
- **Correlation** resolves from `ctxutil.WithCorrelationID`, then the request ID; without either, the store generates a UUIDv7 per Command transaction. Consumer runtimes put the event's correlation into the handler context, the scheduler starts a fresh one per run, and the Inbox uses `inbox:<source>:<key>`.
- **Causation** is only ever the `EventID` of the event that caused this one. Reactors set it; commands from users, the scheduler and the Inbox leave it NULL.
- **`recorded_at`** is the store clock (`WithClock`) at append. There is no caller override; a business time belongs in the event payload.

## Considered Options

- **Actor from `Claims.UserID` only, system actors via a fake user ID.** This was rejected because it makes `UserID` mean "authenticated user" in one place and "whoever acts" in another, and an append without a user would silently store an empty actor.
- **Free-text causation** (`event:<uuid>`, `inbox:<source>:<key>`). This was rejected because causation then no longer reliably points at a row in the log; the Inbox's provenance is already in the actor and the correlation.
- **Caller-supplied `occurred_at`** (an `OccurredOn` override). This was rejected because a timestamp column that is sometimes wall clock and sometimes business time is unreliable for both.
- **Correlation required, never generated.** This was rejected because it makes every prototype and test set it by hand for no gain.

## Consequences

- The columns live in an append-only log (ADR-0009), so their meaning cannot be changed for existing events.
- Code that calls `Repository.Create`/`Update` outside a request (scripts, tests) must set an actor or tenant-carrying claims explicitly.
- The Reactor API's `system:<name>` actor becomes `system:reactor:<name>`.
