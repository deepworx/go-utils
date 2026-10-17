-- go-utils Postgres schema (complete current state).
--
-- The library never runs DDL. Apply this file with your own migration tool
-- (see ADR-0003). It is written for the `public` schema; to use another
-- schema, replace every `public.` qualifier and the derived notify channel
-- names. Each later schema change ships as an upgrade snippet in
-- docs/postgres/upgrades/NNNN_<description>.sql.
--
-- Sections are grouped by the package that owns the objects.

-- Guard: PostgreSQL 15 or newer is required.
DO $$
BEGIN
    IF current_setting('server_version_num')::int < 150000 THEN
        RAISE EXCEPTION 'go-utils schema requires PostgreSQL 15 or newer, found %',
            current_setting('server_version');
    END IF;
END
$$;
