-- Run after setup.sql: psql -v ON_ERROR_STOP=1 -f bench.sql
\set xmin '''2000000'''
\pset pager off
SET max_parallel_workers_per_gather = 0;  -- keep plans comparable and readable

-- Read query, plain form (what the ticket describes)
PREPARE read_plain(text[], xid8, bigint, int) AS
  SELECT global_position, transaction_id, event_type FROM events
  WHERE event_type = ANY($1)
    AND (transaction_id, global_position) > ($2, $3)
    AND transaction_id < '2000000'::xid8
  ORDER BY transaction_id, global_position
  LIMIT $4;

-- Read query, one ordered index range per type, merged on top
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

-- Pending count, plain form with saturation at cap+1
PREPARE pending_plain(text[], xid8, bigint, int) AS
  SELECT count(*) FROM (
    SELECT 1 FROM events
    WHERE event_type = ANY($1)
      AND (transaction_id, global_position) > ($2, $3)
      AND transaction_id < '2000000'::xid8
    LIMIT $4 + 1
  ) s;

-- Pending count, per type, saturating
PREPARE pending_lateral(text[], xid8, bigint, int) AS
  SELECT least(sum(n), $4 + 1)::bigint FROM unnest($1) AS t(event_type)
  CROSS JOIN LATERAL (
    SELECT count(*) AS n FROM (
      SELECT 1 FROM events
      WHERE event_type = t.event_type
        AND (transaction_id, global_position) > ($2, $3)
        AND transaction_id < '2000000'::xid8
      LIMIT $4 + 1
    ) s
  ) c;

-- Claim-time check: any event of the added types at or below the Cursor?
PREPARE claim_exists(text[], xid8, bigint) AS
  SELECT EXISTS (
    SELECT 1 FROM events
    WHERE event_type = ANY($1)
      AND (transaction_id, global_position) <= ($2, $3)
  );

-- Await: any matching event in (cursor, min]?
PREPARE await_exists(text[], xid8, bigint, xid8, bigint) AS
  SELECT EXISTS (
    SELECT 1 FROM events
    WHERE event_type = ANY($1)
      AND (transaction_id, global_position) > ($2, $3)
      AND (transaction_id, global_position) <= ($4, $5)
  );

\echo '=== S1 steady poll: rare type, Cursor at its last event, ~5M non-matching above'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{Rare.Early}', '17666', 50000, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral('{Rare.Early}', '17666', 50000, 100);

\echo '=== S2 steady poll: two rare types, nothing pending'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{Rare.Early,Rare.Spread}', '1664335', 4990007, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral('{Rare.Early,Rare.Spread}', '1664335', 4990007, 100);

\echo '=== S3 catch-up from zero: two common types (~19% of the log)'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{Order.Placed,Customer.Updated}', '0', 0, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral('{Order.Placed,Customer.Updated}', '0', 0, 100);

\echo '=== S4 catch-up mid-log: two common types'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{Order.Placed,Customer.Updated}', '834333', 2500000, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral('{Order.Placed,Customer.Updated}', '834333', 2500000, 100);

\echo '=== S5 catch-up from zero: two sparse types (~0.06% of the log)'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{Rare.Spread,Misc.T3}', '0', 0, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral('{Rare.Spread,Misc.T3}', '0', 0, 100);

\echo '=== S6 catch-up from zero: single type'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{Order.Placed}', '0', 0, 100);

\echo '=== S7 catch-up from zero: 5 types incl. the hottest (~99% of the log)'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_plain('{Hot.Ping,Order.LineAdded,Order.Placed,Customer.Updated,Rare.Spread}', '0', 0, 100);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE read_lateral('{Hot.Ping,Order.LineAdded,Order.Placed,Customer.Updated,Rare.Spread}', '0', 0, 100);

\echo '=== P1 pending, cap 10000: rare consumer far behind (0 pending)'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE pending_plain('{Rare.Early}', '17666', 50000, 10000);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE pending_lateral('{Rare.Early}', '17666', 50000, 10000);

\echo '=== P2 pending, cap 10000: new version catching up (saturates)'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE pending_plain('{Order.Placed,Customer.Updated}', '0', 0, 10000);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE pending_lateral('{Order.Placed,Customer.Updated}', '0', 0, 10000);

\echo '=== P3 pending, cap 10000: sparse types mid-log (~1.5k pending)'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE pending_plain('{Rare.Spread,Misc.T3}', '834333', 2500000, 10000);
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE pending_lateral('{Rare.Spread,Misc.T3}', '834333', 2500000, 10000);

\echo '=== C1 claim EXISTS: added types, one never written, Cursor mid-log'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE claim_exists('{Misc.T3,Brand.New}', '834333', 2500000);
\echo '=== C2 claim EXISTS: only never-written types'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE claim_exists('{Brand.New,Brand.Other}', '834333', 2500000);

\echo '=== A1 Await: rare consumer far behind, min = head of another type'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) EXECUTE await_exists('{Rare.Early}', '17666', 50000, '1667666', 5000000);
