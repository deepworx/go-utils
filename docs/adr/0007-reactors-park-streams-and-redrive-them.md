# Reactors park streams and redrive them on a schedule

A Reactor that fails to react to an event, once its retries are spent or at once for `consumer.Permanent`, a panic or a decode error, does not go Stopped. It **parks** the event under `(reactor, tenant, stream)` in `parked_events` and its Cursor moves on. Every later event of that Stream queues behind the parked one unsent, so the Stream's order holds while every other Stream keeps flowing. Parked heads are **redriven** automatically on a jittered schedule (default 1m, 5m, 30m, 2h, 12h), and after the last step the hold is marked exhausted until an operator calls `Redrive` or `Discard`. Only a failure to write the park list goes back to the Consumer core, which retries and finally Stops, because advancing past an unrecorded event would lose it.

## Considered Options

- **Stop the Reactor like any Consumer.** This was rejected because a Reactor's failures mostly come from the external world (a 503, a 422 for one record). Halting every tenant's side effects for one bad record makes the blast radius the whole service.
- **Skip the failed event (dead-letter) and keep the Stream flowing.** This was rejected because later events of the same Stream would then reach the external system ahead of an earlier one, for example a rename sent before the create it depends on.
- **Manual redrive only.** This was rejected because a short outage parks every Stream active in that window, and each one would need a human. With the schedule, outages heal themselves, and exhaustion still surfaces real poison events.

## Consequences

- Reactors are always `OwnTransaction` and therefore at-least-once. The library offers no dedupe: commands rely on aggregate state guards, and outbound calls pass `Envelope.EventID` as their idempotency key.
- `StartAt` (`FromNow` or `FromBeginning`) is required. The runtime seeds the Cursor at the first claim, so history is never replayed into side effects by accident.
- `Discard` keeps the row with `discarded_at` and `discarded_by`, so a skipped side effect stays traceable. The service owns cleanup.
- `Drain` also runs the redrives that are due at call time, so tests under `testing/synctest` can advance time and watch a hold heal.
- `docs/postgres/schema.sql` gains `parked_events`, and the lock owner reports the parked-stream, parked-event and exhausted gauges.
