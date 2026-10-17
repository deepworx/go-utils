-- Sketch of consumer_pending(name, cap) on the per-type form. The cursors table
-- here is a stand-in; the real columns are decided by the schema ticket.
CREATE TABLE IF NOT EXISTS consumer_cursors (
  name          text PRIMARY KEY,
  last_xid      xid8   NOT NULL DEFAULT '0',
  last_position bigint NOT NULL DEFAULT 0,
  event_types   text[] NOT NULL DEFAULT '{}'
);

-- Returns the number of events the Consumer can reach, saturating at cap + 1
-- ("more than cap"). Raises when the Consumer has no Cursor row.
CREATE OR REPLACE FUNCTION consumer_pending(p_name text, p_cap int)
RETURNS bigint LANGUAGE plpgsql STABLE AS $$
DECLARE
  c      consumer_cursors%ROWTYPE;
  v_xmin xid8 := pg_snapshot_xmin(pg_current_snapshot());
  n      bigint;
BEGIN
  SELECT * INTO c FROM consumer_cursors WHERE name = p_name;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'consumer % has no cursor row', p_name USING ERRCODE = 'no_data_found';
  END IF;
  IF cardinality(c.event_types) = 0 THEN
    SELECT count(*) INTO n FROM (
      SELECT 1 FROM events e
      WHERE (e.transaction_id, e.global_position) > (c.last_xid, c.last_position)
        AND e.transaction_id < v_xmin
      LIMIT p_cap + 1
    ) s;
  ELSE
    SELECT least(coalesce(sum(k.n), 0), p_cap + 1) INTO n
    FROM unnest(c.event_types) AS t(event_type)
    CROSS JOIN LATERAL (
      SELECT count(*) AS n FROM (
        SELECT 1 FROM events e
        WHERE e.event_type = t.event_type
          AND (e.transaction_id, e.global_position) > (c.last_xid, c.last_position)
          AND e.transaction_id < v_xmin
        LIMIT p_cap + 1
      ) s
    ) k;
  END IF;
  RETURN n;
END $$;
