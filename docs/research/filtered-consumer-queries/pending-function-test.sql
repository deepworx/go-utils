-- The synthetic transaction ids lie above the container's real xmin, so every
-- count is 0 here; this checks that the function runs and how fast. Saturation
-- and plan shape are covered by P1-P3 in bench.sql.
\timing on
INSERT INTO consumer_cursors VALUES
  ('rare@v1',   '17666',  50000,   '{Rare.Early}'),
  ('legacy@v1', '667666', 2000000, '{Legacy.Thing}'),
  ('orders@v2', '0',      0,       '{Order.Placed,Customer.Updated}')
ON CONFLICT (name) DO NOTHING;
SELECT name, consumer_pending(name, 10000) FROM consumer_cursors ORDER BY name;
SELECT consumer_pending('missing', 10);
-- Plan shape with the real xmin expression instead of the literal used in bench.sql
EXPLAIN (COSTS OFF)
  SELECT 1 FROM events WHERE event_type = 'Rare.Early'
    AND (transaction_id, global_position) > ('17666'::xid8, 50000)
    AND transaction_id < pg_snapshot_xmin(pg_current_snapshot())
  ORDER BY transaction_id, global_position LIMIT 100;
