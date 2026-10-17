-- Adds types whose events are clustered in time, which column statistics cannot see:
-- Legacy.Thing: ~24% of the log, written only in the first 2M positions (a retired type)
-- New.Thing:    ~1.2% of the log, written only after position 4.9M (a recently added type)
UPDATE events SET event_type = 'Legacy.Thing' WHERE event_type = 'Hot.Ping' AND global_position <= 2000000;
UPDATE events SET event_type = 'New.Thing'    WHERE event_type = 'Hot.Ping' AND global_position >  4900000;
VACUUM (ANALYZE) events;
