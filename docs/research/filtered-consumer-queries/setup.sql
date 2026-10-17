-- Synthetic event log for DWX-227. ~5M events, skewed event types.
-- transaction_id is synthetic (3 events per transaction), so queries bound it
-- with a literal :xmin instead of pg_snapshot_xmin(pg_current_snapshot()).
DROP TABLE IF EXISTS events;
CREATE TABLE events (
  global_position bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  transaction_id  xid8        NOT NULL,
  tenant_id       text        NOT NULL,
  stream_id       text        NOT NULL,
  stream_version  int         NOT NULL,
  event_type      text        NOT NULL,
  payload         jsonb       NOT NULL,
  occurred_on     timestamptz NOT NULL
);

SELECT setseed(0.227);
INSERT INTO events (transaction_id, tenant_id, stream_id, stream_version, event_type, payload, occurred_on)
SELECT ((1000 + g / 3)::text)::xid8,
       't' || (g % 20),
       's' || (g % 200000),
       g / 200000,
       CASE
         -- Rare.Early: 100 events, all inside the first 50k positions, none after
         WHEN g <= 50000 AND g % 500 = 0 THEN 'Rare.Early'
         -- Rare.Spread: ~500 events spread across the whole log
         WHEN g % 10000 = 7 THEN 'Rare.Spread'
         WHEN r < 0.60 THEN 'Hot.Ping'
         WHEN r < 0.80 THEN 'Order.LineAdded'
         WHEN r < 0.90 THEN 'Order.Placed'
         WHEN r < 0.99 THEN 'Customer.Updated'
         ELSE 'Misc.T' || (g % 20)
       END,
       jsonb_build_object('id', g, 'note', repeat('x', 120)),
       timestamptz '2025-01-01' + g * interval '1 second'
FROM (SELECT g, random() AS r FROM generate_series(1, 5000000) g) s;

CREATE INDEX events_log_order  ON events (transaction_id, global_position);
CREATE INDEX events_type_order ON events (event_type, transaction_id, global_position);
VACUUM (ANALYZE) events;
