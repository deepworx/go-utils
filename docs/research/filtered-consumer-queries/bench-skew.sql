-- Run after setup.sql and skew.sql. Reuses the statements of bench.sql.
\pset pager off
SET max_parallel_workers_per_gather = 0;
PREPARE read_plain(text[], xid8, bigint, int) AS
  SELECT global_position, transaction_id, event_type FROM events
  WHERE event_type = ANY($1)
    AND (transaction_id, global_position) > ($2, $3)
    AND transaction_id < '2000000'::xid8
  ORDER BY transaction_id, global_position
  LIMIT $4;
PREPARE read_lateral(text[], xid8, bigint, int) AS
  SELECT e.global_position, e.transaction_id, e.event_type
  FROM unnest($1) AS t(event_type)
  CROSS JOIN LATERAL (
    SELECT global_position, transaction_id, event_type FROM events
    WHERE event_type = t.event_type
      AND (transaction_id, global_position) > ($2, $3)
      AND transaction_id < '2000000'::xid8
    ORDER BY transaction_id, global_position
    LIMIT $4
  ) e
  ORDER BY e.transaction_id, e.global_position
  LIMIT $4;
PREPARE pending_plain(text[], xid8, bigint, int) AS
  SELECT count(*) FROM (
    SELECT 1 FROM events
    WHERE event_type = ANY($1)
      AND (transaction_id, global_position) > ($2, $3)
      AND transaction_id < '2000000'::xid8
    LIMIT $4 + 1
  ) s;

SELECT max(global_position) AS last_legacy, max(transaction_id) AS last_legacy_xid FROM events WHERE event_type = 'Legacy.Thing';

\echo '=== K1 steady poll: retired type (24% in stats), Cursor at its last event, 3M non-matching above'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{Legacy.Thing}', '667666', 2000000, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral('{Legacy.Thing}', '667666', 2000000, 100);

\echo '=== K2 steady poll: retired type + rare type'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{Legacy.Thing,Rare.Early}', '667666', 2000000, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral('{Legacy.Thing,Rare.Early}', '667666', 2000000, 100);

\echo '=== K3 catch-up from zero: recently added type (1.2% in stats, all after 4.9M)'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{New.Thing}', '0', 0, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral('{New.Thing}', '0', 0, 100);

\echo '=== K4 catch-up from zero: recently added type + sparse type'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{New.Thing,Misc.T3}', '0', 0, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral('{New.Thing,Misc.T3}', '0', 0, 100);

\echo '=== K5 pending, cap 10000: retired type, 0 pending'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE pending_plain('{Legacy.Thing}', '667666', 2000000, 10000);

\echo '=== G* generic plans (what a driver-side prepared statement can end up with)'
SET plan_cache_mode = force_generic_plan;
\echo '=== G1 generic: S1 rare type far behind'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{Rare.Early}', '17666', 50000, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral('{Rare.Early}', '17666', 50000, 100);
\echo '=== G2 generic: catch-up from zero, two common types'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{Order.Placed,Customer.Updated}', '0', 0, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral('{Order.Placed,Customer.Updated}', '0', 0, 100);
\echo '=== G3 generic: pending, rare type far behind'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE pending_plain('{Rare.Early}', '17666', 50000, 10000);
