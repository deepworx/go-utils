# Read and lag queries for event-type-filtered Consumers

Research for Linear DWX-227 (map DWX-205). It builds on DWX-219 (filtered
Cursors don't advance past non-matching events) and DWX-220 / ADR-0006
(Consumers filter by event type; the filter set is stored on the Cursor row and
checked at claim time).

Evidence: EXPLAIN (ANALYZE, BUFFERS) on PostgreSQL 15.19, 17.11 and 18.6
(official `postgres:*` images) against a synthetic 5M-event log, plus the
PostgreSQL 17/18 release notes and the nbtree commit message. The scripts and
raw outputs are in [`filtered-consumer-queries/`](filtered-consumer-queries/);
`run.sh` reproduces everything.

## Answer

1. **Ship two indexes** in `docs/postgres/schema.sql`:
   `events (transaction_id, global_position)` for unfiltered Consumers and
   `events (event_type, transaction_id, global_position)` for filtered ones.
2. **Do not ship the plain `event_type = ANY($types) … ORDER BY … LIMIT n`
   query.** It is correct, but the planner chooses between two plans that each
   have a pathological case. In the worst case it walks every non-matching
   event between the Cursor and the head, costing 0.35–0.94 s per poll on 3–5M
   rows, all in memory. That happens on every version tested, 18 included.
3. **Read one ordered index range per type and merge them:**
   `unnest($types) CROSS JOIN LATERAL (… WHERE event_type = t … ORDER BY … LIMIT n)`,
   then `ORDER BY … LIMIT n` on top. Every scenario tested cost 0.01–0.2 ms
   with this form, on all three versions, with custom and generic plans alike.
   The cost depends on the number of types and the batch size, not on how far
   the Cursor is behind or how much non-matching traffic there is.
4. **PG 15 vs PG 18 makes no difference here.** Plans and timings were
   identical across 15, 17 and 18. The PG 17 nbtree SAOP work and the PG 18
   skip scan do not merge several `event_type` runs into log order, so no
   version serves multi-value `ANY` in log order without a sort.
5. **No bounded advance-on-skip is needed.** With the per-type form, a rare-type
   Consumer far behind costs about four buffer hits per poll. DWX-219's
   rejection stands.
6. **The claim-time `EXISTS`, the Await check and `consumer_pending(name, cap)`
   should use the same per-type form.** Each then costs O(types) index descents
   plus at most `cap+1` index-only entries per type. The plain `ANY` versions
   can degrade to a sequential scan of the whole log (0.45 s in test A2).

## Setup

- `events`: `global_position bigint identity PK`, `transaction_id xid8`,
  `event_type text`, tenant/stream columns, a ~150-byte `jsonb` payload. There
  are 5,000,000 rows and three events per synthetic transaction (see
  [`setup.sql`](filtered-consumer-queries/setup.sql)).
- Type mix: `Hot.Ping` 60 %, `Order.LineAdded` 20 %, `Order.Placed` 10 %,
  `Customer.Updated` 9 %, 20 `Misc.T*` types at about 0.05 % each,
  `Rare.Spread` (500 events spread evenly), and `Rare.Early` (100 events, all
  in the first 50k positions).
- [`skew.sql`](filtered-consumer-queries/skew.sql) then adds two types whose
  events are clustered in time, which column statistics cannot see:
  - `Legacy.Thing`: about 24 % of the log, written only in the first 2M
    positions (a retired type).
  - `New.Thing`: about 1.2 % of the log, written only after position 4.9M (a
    recently introduced type).
- Both indexes are present and the table is `VACUUM ANALYZE`d. Parallel query
  is off to keep the plans readable. Timings come from the second (warm) run.
- Transaction IDs are synthetic, so the benchmarks bound the read with a
  literal `transaction_id < '2000000'::xid8` instead of
  `pg_snapshot_xmin(pg_current_snapshot())`. `pending-function-test.sql`
  confirms that the real expression is used as an Index Cond in the same
  position (`out-function-pg*.txt`).

## Findings

### 1. Why the plain `ANY` query is fragile

The planner has two candidate plans, and the choice depends on statistics.

- **Log-order index plus a filter** (`events_log_order`, `Filter: event_type = ANY(...)`).
  This plan walks the log from the Cursor and stops after `n` matches. It
  wins when the types are dense above the Cursor. When they are not, it walks
  to xmin, fetching each heap row only to throw it away, because `event_type`
  is not in that index.
- **Type index plus a sort** (`events_type_order`, `Index Cond: event_type = ANY(...)`,
  then `Sort`). With more than one value, the index returns
  `(event_type, transaction_id, global_position)` order, which is not log
  order. It therefore reads **every** matching row above the Cursor and
  top-N-sorts them. One example: S5 read 2,964 rows to return 100. On a
  catch-up of common types this grows with the backlog on every batch.

Statistics cannot see where in the log a type's events sit. The bad choices
come from that, plus generic plans:

| Case (PG 15 / 17 / 18) | Plain `ANY` | Per-type `LATERAL` |
|---|---|---|
| K1 steady poll, retired type (24 % in stats), 3M non-matching above Cursor | log index, **3,000,000 rows filtered, 342 / 372 / 437 ms** | 0.07 ms |
| K3 new-version catch-up of a recently added type (1.2 % in stats, all after 4.9M) | log index, **4,900,066 rows filtered, 667 / 699 / 854 ms** | 0.07 ms |
| K4 same plus a sparse type | log index, 202,463 rows filtered, 34 / 41 / 41 ms | 0.09–0.10 ms |
| G1 generic plan, rare type far behind (`plan_cache_mode = force_generic_plan`) | log index, **4,950,000 rows filtered, 655 / 709 / 937 ms** | 0.04 ms |
| S1 custom plan, rare type far behind | type index, 0.04 ms | 0.02 ms |
| S3/S4 catch-up, two common types | log index, 0.09–0.13 ms | 0.07–0.08 ms |
| S7 five types covering 99 % of the log | log index, 0.02 ms | 0.13–0.16 ms |

Sources: `out-skew-pg{15,17,18}.txt` and `out-pg{15,17,18}.txt`.

The generic-plan row matters in practice. A prepared statement switches to a
generic plan after five executions if that plan's estimated cost is not much
worse than the custom plans'
([PREPARE, "Notes"](https://www.postgresql.org/docs/current/sql-prepare.html)).
pgx's default exec mode caches prepared statements, and a Consumer polls the
same statement forever. The generic plan cannot see which types are bound, so
it picked the log-order walk for a rare-type Consumer.

### 2. The per-type form is planner-proof

```sql
SELECT e.*
FROM unnest($1::text[]) AS t(event_type)
CROSS JOIN LATERAL (
  SELECT * FROM events
  WHERE event_type = t.event_type
    AND (transaction_id, global_position) > ($2, $3)
    AND transaction_id < pg_snapshot_xmin(pg_current_snapshot())
  ORDER BY transaction_id, global_position
  LIMIT $4
) e
ORDER BY e.transaction_id, e.global_position
LIMIT $4;
```

- Inside the lateral, `event_type = <one value>` is an equality on the leading
  column. The type index therefore yields `(transaction_id, global_position)`
  order directly, and the planner always picks a bounded index range: start at
  the Cursor, stop at `n` rows or at xmin. The row comparison and the xmin
  bound both appear as `Index Cond`, not as a filter, on all three versions.
- The outer sort handles at most `types × n` rows. In S7 that was 500 rows,
  top-N heapsorted in 34 kB.
- It is correct. Each type contributes its first `n` rows above the Cursor, so
  the overall first `n` rows in log order are among them.
- It is one fixed SQL text for any number of types. A `UNION ALL` per type
  would let PostgreSQL use Merge Append, but that needs dynamic SQL and a
  separate plan per type count.
- Its cost is `types × n` heap fetches instead of `n`. Test R1 compared full
  rows inside the lateral against keys first followed by a PK join: 203 vs 424
  buffer hits at 5 types × 100. Selecting the rows inside the lateral is
  better at this size. For Consumers with dozens of types and large batches,
  keys first becomes the better choice. Treat this as a tuning knob, not a
  decision.
- Unfiltered Consumers (empty set) use a separate statement without the type
  predicate on `events_log_order`. Do not fold this into one statement with
  `cardinality($1) = 0 OR …`: an `OR` across parameters is exactly what
  generic plans handle badly.

### 3. What PG 17 and PG 18 changed, and why it does not help here

- PG 17: "Allow btree indexes to more efficiently find a set of values, such as
  those supplied by `IN` clauses using constants"
  ([17.0 release notes](https://www.postgresql.org/docs/release/17.0/), commit
  [5bf748b86](https://git.postgresql.org/gitweb/?p=postgresql.git;a=commit;h=5bf748b86bc6786a3fc57fc7ce296c37da6564b0)).
  The commit message says the planner had been avoiding "path keys on a
  multicolumn index path, with a SAOP clause on any attribute beyond the
  first", and that it now generates path keys regardless of SAOP clauses,
  because "index scans … with lower-order SAOP equalities reliably output
  tuples in index order". That fixes ordering for SAOP on **lower-order**
  columns. Our SAOP is on the **leading** column, and the order we need is
  across its values, which no btree scan provides: the index order is
  `(type, xid, pos)`, not `(xid, pos)`.
- PG 18: "Allow skip scans of btree indexes … when there are no restrictions
  on the first or early indexed columns"
  ([18.0 release notes](https://www.postgresql.org/docs/release/18.0/)). This
  covers the opposite situation: the leading column is unconstrained. It does
  not merge runs either. PG 18 also converts `OR` clauses and some
  `IN (VALUES …)` to `= ANY` (same notes), which does not affect an
  `= ANY($1)` that is already in that form.
- Empirically, every plan in `out-pg{15,17,18}.txt`, `out-skew-*` and
  `out-extra-*` have the same shape on 15, 17 and 18. Even a single-element
  `= ANY('{Order.Placed}')` (S6) is not treated as an equality for ordering on
  any of the three versions. The per-type form uses `=` and avoids that too.

### 4. A rare-type Consumer far behind lots of non-matching traffic

| Situation | Plain `ANY` | Per-type form |
|---|---|---|
| Steady-state poll, nothing pending (S1, S2, K1, K2, G1) | 0.01–0.04 ms when stats favour the type index; 0.34–0.94 s when the type is common in stats or the plan is generic | 0.01–0.07 ms, 4–8 buffer hits |
| Catch-up of a new Projection version (S3–S5, S7, K3, K4) | 0.02–0.33 ms when estimates hold; up to 0.85 s per batch for a recently added type | 0.06–0.16 ms per batch of 100 |
| Await, "no matching event in `(cursor, min]`" (A1, A2) | 0.004 ms, or a **Seq Scan of 5M rows (445–496 ms)** for a retired type | 0.02–0.04 ms |

With the per-type form, the distance between the Cursor and the head does not
affect cost. Each poll descends each type's index once and stops at xmin.

### 5. Bounded advance-on-skip: not needed

Advance-on-skip only helps a plan that walks non-matching events, and the
per-type form never does. Adding it would bring back the costs DWX-219 rejected
(a Cursor write on polls that handled nothing, and a blurred "last handled")
for no gain. Lag and Await stay correct without it, because both count with the
read's exact predicate (DWX-219, ADR-0006).

### 6. Claim-time check: one indexed `EXISTS` per added type

```sql
SELECT EXISTS (
  SELECT 1 FROM unnest($1::text[]) AS t(event_type)
  WHERE EXISTS (
    SELECT 1 FROM events
    WHERE event_type = t.event_type
      AND (transaction_id, global_position) <= ($2, $3)
  )
);
```

- Per-type form: 4–5 buffer hits, 0.01–0.05 ms, whether the type is
  never-written, exists only above the Cursor, or is retired (C3, C4).
- Plain `ANY` form: fine in C1–C3, but in C4 it chose a Seq Scan whose cost
  depends on where the first match sits on the heap. It removed 52 rows on
  PG 15 and 260,803 rows (35 ms) on PG 18 with the same data. That is luck, not
  a plan.
- It runs once per claim, so even the bad case is tolerable. The per-type form
  still costs nothing extra and keeps every query on one pattern.

### 7. Pending count and `consumer_pending(name, cap)`

- Pending is the read's predicate counted with `LIMIT cap+1` per type, summed,
  then `least(sum, cap+1)`. A result of `cap+1` means "more than cap". Measured
  with cap 10,000 (P1–P3):
  - 0 pending: 0.01–0.03 ms.
  - Saturated: about 1–2 ms (two types × 10,001 index-only entries, 140
    buffers).
  - Sparse mid-log: 0.15 ms.
- All counts were index-only (`Heap Fetches: 0`), because the type index
  contains every column the predicate needs. Index-only scans depend on the
  visibility map. On an append-only table, insert-triggered autovacuum (PG 13+)
  keeps it current. Only the not-yet-vacuumed tail costs heap visits.
- The plain `ANY` count happened to pick the type index in every run (P1–P3,
  K5, G3). It has the same planner exposure as the read, though, so use the
  per-type form for consistency.
- A sketch of the function is in
  [`pending-function.sql`](filtered-consumer-queries/pending-function.sql). It
  is plpgsql and `STABLE`, branches on the empty set (unfiltered uses
  `events_log_order`), and raises `no_data_found` for a missing Cursor row. It
  is verified to run on 15 and 18. The `consumer_cursors` columns are
  stand-ins until the schema ticket names them. Name the local variable
  something other than `xmin`, because `xmin` is a system column and plpgsql
  rejects it as ambiguous.

### 8. Index cost

On the 5M-row test table (sizes include some bloat from the skew `UPDATE`s),
the heap was 1.5 GB, the PK 152 MB, `events_log_order` 214 MB and
`events_type_order` 348 MB. The type index is the largest, because of the
`text` key. Every append maintains three btrees. Both secondary indexes stay
because unfiltered Consumers and `EventLogHead`-style queries need log order
without a type, and PG 18's skip scan cannot deliver log order from the type
index. Short, namespaced type names (`Todo.Task.Opened`) keep the type index
small. Btree deduplication (PG 13+) does not apply, because
`(event_type, xid, pos)` keys are unique.

## Recommendation for the spec

- `schema.sql`:
  ```sql
  CREATE INDEX events_log_order  ON events (transaction_id, global_position);
  CREATE INDEX events_type_order ON events (event_type, transaction_id, global_position);
  ```
- Filtered read, pending, Await and claim check all use the per-type
  `unnest … CROSS JOIN LATERAL` form with `event_type = t.event_type`.
  Unfiltered Consumers use separate statements on `events_log_order`. Never
  use `event_type = ANY($types)` with `ORDER BY … LIMIT`.
- `consumer_pending(name, cap)` ships in `schema.sql` on the same pattern
  (sketch above). It saturates at `cap+1` and raises when the Cursor row is
  missing.
- No advance-on-skip. No PG-version-specific SQL is needed: PG 15 behaves like
  PG 18 for every query here.
- An integration test should assert the plan shape, not just the result.
  Examples: `EXPLAIN` of the read for a rare type with a generic plan contains
  no `Filter: (event_type`, or a "retired type" fixture stays under a buffer
  budget. A refactor back to `= ANY` would then fail CI.
