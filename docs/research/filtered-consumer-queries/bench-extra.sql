-- Run after setup.sql and skew.sql: claim/Await edge cases and heap fetches of the read.
\pset pager off
SET max_parallel_workers_per_gather = 0;
PREPARE claim_exists(text[], xid8, bigint) AS
  SELECT EXISTS (
    SELECT 1 FROM events
    WHERE event_type = ANY($1)
      AND (transaction_id, global_position) <= ($2, $3)
  );
PREPARE claim_lateral(text[], xid8, bigint) AS
  SELECT EXISTS (
    SELECT 1 FROM unnest($1) AS t(event_type)
    WHERE EXISTS (
      SELECT 1 FROM events
      WHERE event_type = t.event_type
        AND (transaction_id, global_position) <= ($2, $3)
    )
  );
PREPARE await_exists(text[], xid8, bigint, xid8, bigint) AS
  SELECT EXISTS (
    SELECT 1 FROM events
    WHERE event_type = ANY($1)
      AND (transaction_id, global_position) > ($2, $3)
      AND (transaction_id, global_position) <= ($4, $5)
  );
PREPARE await_lateral(text[], xid8, bigint, xid8, bigint) AS
  SELECT EXISTS (
    SELECT 1 FROM unnest($1) AS t(event_type)
    WHERE EXISTS (
      SELECT 1 FROM events
      WHERE event_type = t.event_type
        AND (transaction_id, global_position) > ($2, $3)
        AND (transaction_id, global_position) <= ($4, $5)
    )
  );
-- Full rows: every per-type candidate is fetched from the heap
PREPARE read_lateral_rows(text[], xid8, bigint, int) AS
  SELECT e.* FROM unnest($1) AS t(event_type)
  CROSS JOIN LATERAL (
    SELECT * FROM events
    WHERE event_type = t.event_type
      AND (transaction_id, global_position) > ($2, $3)
      AND transaction_id < '2000000'::xid8
    ORDER BY transaction_id, global_position
    LIMIT $4
  ) e
  ORDER BY e.transaction_id, e.global_position
  LIMIT $4;
-- Keys first (index-only), then fetch only the n winning rows
PREPARE read_lateral_keys(text[], xid8, bigint, int) AS
  SELECT ev.* FROM (
    SELECT e.transaction_id, e.global_position
    FROM unnest($1) AS t(event_type)
    CROSS JOIN LATERAL (
      SELECT transaction_id, global_position FROM events
      WHERE event_type = t.event_type
        AND (transaction_id, global_position) > ($2, $3)
        AND transaction_id < '2000000'::xid8
      ORDER BY transaction_id, global_position
      LIMIT $4
    ) e
    ORDER BY e.transaction_id, e.global_position
    LIMIT $4
  ) k
  JOIN events ev ON ev.global_position = k.global_position
  ORDER BY k.transaction_id, k.global_position;

\echo '=== C3 claim: added type exists only above the Cursor (New.Thing), Cursor mid-log'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE claim_exists('{New.Thing}', '834333', 2500000);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE claim_lateral('{New.Thing}', '834333', 2500000);
\echo '=== C4 claim: added retired type + never-written type, Cursor near head'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE claim_exists('{Legacy.Thing,Brand.New}', '1667000', 4998000);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE claim_lateral('{Legacy.Thing,Brand.New}', '1667000', 4998000);
\echo '=== A2 Await: retired type, Cursor at its last event, min at head'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE await_exists('{Legacy.Thing}', '667666', 2000000, '1667666', 5000000);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE await_lateral('{Legacy.Thing}', '667666', 2000000, '1667666', 5000000);
\echo '=== R1 read 5 common types from zero: full rows vs keys first'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral_rows('{Legacy.Thing,Order.LineAdded,Order.Placed,Customer.Updated,Rare.Spread}', '0', 0, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral_keys('{Legacy.Thing,Order.LineAdded,Order.Placed,Customer.Updated,Rare.Spread}', '0', 0, 100);
\echo '=== G4 generic plans for claim and Await'
SET plan_cache_mode = force_generic_plan;
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE claim_exists('{New.Thing}', '834333', 2500000);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE await_exists('{Legacy.Thing}', '667666', 2000000, '1667666', 5000000);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE await_lateral('{Legacy.Thing}', '667666', 2000000, '1667666', 5000000);
