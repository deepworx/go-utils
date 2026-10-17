# Do filtered Cursors advance past non-matching events?

Research for Linear DWX-219 (map DWX-205). It feeds the projection switch guard
in DWX-216 and the library's `Lag` gauge.

Source: ariadne-cpq PR 230, branch `Daniel133/otc-373-runtime-core`, commit
`3420322`. Paths below are relative to `services/api/`. Line numbers refer to
that commit.

## Answer

**No.** A Consumer filtered by stream type does not advance past events it
filters out. Its stored Cursor stays at the last *matching* event it handled.
CPQ's `Lag` does not raise false positives because of this: it counts only
reachable events that match the same filter, so `pending == 0` means "caught up
for this consumer" however far the unfiltered head has moved. The weak spots
are measures that compare the Cursor with the *unfiltered* head. The readwait
gate is one of them when it is handed a position from a stream type the
projection does not read.

## Findings

### 1. The filter is applied in SQL, so filtered-out events are never read

- `ReadEventsAfter` filters with
  `(@all_stream_types OR stream_type = ANY(@stream_types))`. The lower bound is
  the Cursor and the upper bound is `transaction_id < pg_snapshot_xmin(pg_current_snapshot())`.
  See `db/queries/eventstore.sql:103-107`.
- `PgSource.ReadAfter` passes the consumer's `StreamTypes` through, and an empty
  list means "all". See `internal/infrastructure/eventstore/pg_source.go:29-36`.
- The in-memory source does the same in Go. See `lib/consumer/memory.go:57`.

### 2. The Cursor is saved only at the position of an event that was returned

- Transactional mode returns early on an empty batch, so nothing is saved when
  every new event was filtered out. See `lib/consumer/runner.go:301-303`.
- Batch handler: it saves `batch[len(batch)-1].Cursor`. See `lib/consumer/runner.go:311`.
- Per-event handler: it saves the cursor of the last handled envelope. See
  `lib/consumer/runner.go:314-321`.
- OwnTransaction mode saves `env.Cursor` after each handled event. See
  `lib/consumer/runner.go:347-355`.
- `PgCursors.Save` upserts `(last_xid, last_position)` exactly as given. See
  `lib/consumer/postgres.go:48-61`.

The stored Cursor is therefore always the `(transaction_id, global_position)`
of a matching event, or the zero Cursor.

### 3. The doc comment covers a different case

`lib/consumer/consumer.go:76-82` says "an event this consumer does not handle
still advances the cursor, because the cursor is a position in the log and not
a worklist". That holds for events **inside** the stream-type filter that the
handler ignores, such as event types it has no case for. The runner reads
those events and saves their cursor (point 2). Events **outside** the filter
are never read, so they never move the Cursor. CPQ's `Consumer` has no
event-type filter. Event-type selection happens in handler code, so it does
advance the Cursor.

### 4. `Lag` uses the same filter and bounds as the read

- `EventLag` returns `count(*)` and `min(occurred_on)` under the same three
  predicates as `ReadEventsAfter`: Cursor, xmin and stream type. The comment
  states the intent: "the number a gauge shows is the number of events the
  consumer can actually reach". See `db/queries/eventstore.sql:133-149`.
- `PgSource.Lag` passes the consumer's stream types and returns `(0, 0)` when
  nothing is pending. See `internal/infrastructure/eventstore/pg_source.go:70-84`.
- The telemetry collector calls `Lag(ctx, cursor, cons.StreamTypes)` for each
  registered consumer. See `internal/infrastructure/telemetry/collector.go:191`.
  The runner exposes `Registered()` for exactly this purpose. Lag "is per
  consumer and narrowed by its stream types". See `lib/consumer/runner.go:160-169`.
- The shadow-build swap check `projection.AtHead` is `pending == 0` from `Lag`
  with `b.Detail().StreamTypes`. See `lib/projection/swap.go:77-89`.

So the head moving on with non-matching events adds nothing to `pending`.
**CPQ's `Lag` and `AtHead` have no false-positive risk from filtering.**

### 5. Where the false-positive risk does exist

**a. Comparing with the unfiltered head.** `EventLogHead` returns
`max(transaction_id)` and `max(global_position)` over the whole table, with no
filter and no xmin bound. See `db/queries/eventstore.sql:174-184` and
`pg_source.go:86-99`. CPQ uses it only to seed reactors. If you computed
"distance to head" from it, the result would grow without limit for a filtered
consumer during unrelated traffic. Note also that the two `max()`s may not
belong to the same row.

**b. The readwait gate.** `Gate.Await` returns once
`!snapshot.cursor.Before(min)`. See `internal/infrastructure/readwait/readwait.go:107`.
Otherwise it waits until the 2s budget runs out and returns `Stale: true`. See
`readwait.go:27-28,116-119`. `min` is whatever position the client sends. See
`internal/infrastructure/handler/readstate/readstate.go:13-22`. The quote and
CRM handlers wait on the quote projection's consumer. See
`internal/infrastructure/handler/quote/handler.go:99` and
`internal/infrastructure/handler/crm/handler.go:148-153`. That consumer reads
only the quote stream type. See
`internal/infrastructure/projection/quote/projection.go:123`.

If the client sends the position of a write to another stream type, the quote
consumer's Cursor never reaches it. Every quote event below it was already
projected, yet the request waits the full 2s and is reported stale. It recovers
only when a later quote event lands. Today the risk is latent: it requires a
caller to pass a non-quote position. It is still a real false "lagging" signal
on the read path.

**c. `Activity.Pending`** gets this right. It asks for "a subscribed event
above the cursor", filtered by `event_type = ANY(...)` and bounded by xmin,
rather than comparing with a head. See
`internal/infrastructure/reactors/activity.go:45-69` and lines 54-63 for the
query.

### 6. Concurrency: why position arithmetic is wrong

`lib/eventstore/envelope.go:15-26` explains that `global_position` alone is not
an order: identity values are drawn before commit, so they become visible out
of order. The log is ordered by `(transaction_id, global_position)` and read
strictly below `pg_snapshot_xmin`. It follows that:

- `head.global_position - cursor.global_position` is not an event count. It
  includes non-matching events, sequence gaps from rollbacks and cached
  values, and in-flight rows. It can even be inconsistent with the cursor
  order, for example an event `(xid 99, pos 60)` sorting before a cursor at
  `(xid 100, pos 50)`.
- Any head or lag measure must use the same xmin bound as the read. Without
  it, an open writer transaction looks like consumer lag, which the
  `EventLag` comment states (`eventstore.sql:137-139`). With it, a
  long-running transaction holds the horizon back. Lag can then *under*-count,
  because rows committed by later xids sit above xmin and are invisible to the
  reader and the gauge alike. For a "lags by at most X" guard this errs toward
  passing. That is acceptable because those rows will still be read in order
  after the swap.

## Options for the library's lag / distance-to-head measure

The migration guard is a plain SQL `DO` block that runs before the view switch
and must assert "shadow Cursor lags head by at most X".

### A. Filtered reachable count, as in CPQ

```sql
SELECT count(*) FROM (
  SELECT 1 FROM events e
  WHERE (e.transaction_id, e.global_position) > (c.last_xid, c.last_position)
    AND e.transaction_id < pg_snapshot_xmin(pg_current_snapshot())
    AND (cardinality(c.stream_types) = 0 OR e.stream_type = ANY(c.stream_types))
  LIMIT X + 1
) s;
```

- Pros:
  - Exact, and has no false positives.
  - Identical predicate to the read.
  - Needs no extra writes.
  - `LIMIT X+1` bounds the cost of the guard.
- Cons:
  - The guard needs the consumer's filter. A hand-written `DO` block can
    drift from the Go registration. Fix this by persisting the filter on the
    cursor row.
  - The gauge without `LIMIT` costs O(distance) and needs an index on
    `(stream_type, transaction_id, global_position)`.

### B. Advance-on-skip (store the scanned position)

After a read, save the highest `(xid, pos)` scanned below xmin even when no
event matched. Then the Cursor is comparable with an unfiltered head.

- Pros:
  - "Caught up" can ignore the filter.
  - Readwait works with any write position.
  - A guard needs no filter.
- Cons:
  - Each poll needs a second, unfiltered "max below xmin" query.
  - Idle consumers write a cursor row on every poll. That means WAL and
    churn, and it needs throttling.
  - It blurs "last handled event", which operators and redrives use.
  - An unfiltered count above the cursor still counts non-matching events
    since the last poll, so X has to allow for that noise.

### C. Per-stream-type head table

`stream_heads(stream_type, last_xid, last_position)`, maintained on append.

- Pros:
  - O(1) lookup of the head.
  - Gives an exact "caught up" boolean: the cursor is at or past the max head
    of its types.
- Cons:
  - A hot row per stream type. The row lock is held until commit, so every
    append of that type is serialized. That is a write-throughput regression
    for the core write path.
  - It gives a boolean, not an event count.
  - It must still respect xmin: the stored head can be a row that the reader
    cannot reach yet.

### D. Filtered max position

`max((transaction_id, global_position))` over matching rows below xmin.
Caught up means `cursor >= that`.

- Pros:
  - Cheap with the index from A.
  - Exact boolean.
  - Read-only.
- Cons:
  - Only a boolean. "At most X" needs a count (A) or a time bound.
  - Subtracting positions brings back every problem from point 6.

### Time-based variant

You can bound by age instead of count, using `min(occurred_on)` of the pending
rows as CPQ's `Lag` does. It complements A or D for the gauge. As a guard it
is less predictable than a count, because it depends on clock skew and on
`occurred_on` being the domain time.

## Recommendation

1. **Define lag as option A, "filtered reachable events above the Cursor",**
   with exactly the read's predicate, and report the age next to it as CPQ
   does. Do not offer any API that subtracts positions or compares with an
   unfiltered head.
2. **Persist the Consumer's filter on its cursor row** (`stream_types text[]`,
   written at registration or start). Ship a SQL function such as
   `consumer_pending(consumer text, cap bigint) RETURNS bigint`. Then the
   guard snippet is generic and cannot drift from the Go registration:

   ```sql
   DO $$
   BEGIN
     IF consumer_pending('quotes_v3_shadow', 1000) > 1000 THEN
       RAISE EXCEPTION 'shadow quotes_v3 lags head by more than 1000 events';
     END IF;
   END $$;
   ```

   The function also has to raise when the cursor row is missing, so a guard
   cannot pass for a consumer that never ran.
3. **Do not advance on skip (B), and do not keep a per-type head table (C).**
   A already removes the false positives without extra writes or a hot row.
4. **Fix the read-your-own-write gate with the same idea, not with B.** Treat
   the gate as caught up when the Cursor is `>= min`, **or** when no matching
   event exists in `(cursor, min]` and `min.xid < xmin`. That is one
   `NOT EXISTS` with the read predicate. A `min` from a non-matching stream
   type then returns at once instead of timing out stale.
5. **Document the xmin behaviour.** In-flight events are not counted, and a
   long transaction makes lag under-count. A `DO` guard passing is "at most X
   *reachable* events behind", which is the right meaning for a swap: the
   shadow consumer keeps running after the switch and reads the rest in order.
