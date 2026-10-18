# Consumers filter the log by event type

A Consumer reads only the event types it handles. The set is derived from its Router (`consumer.On[E]`), or declared as `Config.EventTypes` for a raw Handler (empty means every type). There is no stream-type filter in the API. The set is stored on the Cursor row, and lag, `Drain` and `Await` all count with the read's exact predicate. When a release changes the set under an existing Consumer name, the change is checked when the Consumer is claimed:

- Removing types is always accepted.
- Adding types to a Reactor is accepted and applies from the Cursor onwards, so history is never replayed into side effects.
- Adding types to a Projection is accepted only if no event of the added types exists at or below the Cursor. Otherwise the Consumer goes Stopped and the change needs a new Projection version (ADR-0005).

## Considered Options

- **Stream-type filter.** It is coarse: a Consumer reads every event of its streams and ignores the ones it doesn't route. As a result, lag and `Await` count events that never matter, and the filter has to be declared separately from the handlers.
- **Both filters.** Two predicates have to agree across the read, lag, `Await` and SQL, for no gain over event type alone, because event type names already carry their stream (`Todo.Task.Opened`).
- **Treating every filter change as a version bump.** This is simple but forces pointless rebuilds for event types that have never been written, and it is wrong for Reactors, whose new handlers must not replay history.

## Consequences

- `schema.sql` ships an index on `events (event_type, transaction_id, global_position)`. The read, pending, `Await` and the claim-time `EXISTS` use one ordered index range per type (`unnest … CROSS JOIN LATERAL … event_type = t`), never `event_type = ANY(...) ORDER BY … LIMIT`, which walks millions of non-matching rows under clustered types or generic plans. PG 15 behaves the same as PG 18, and no advance-on-skip is needed: a filtered Cursor stays at its last matching event.
- Adding a handler is the common way a filter changes, so the claim-time check runs on every claim and must be cheap (one indexed `EXISTS`).
