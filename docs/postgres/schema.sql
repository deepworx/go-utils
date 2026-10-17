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

-- pkg/scheduler ---------------------------------------------------------------

-- One row per daily job and Business date. finished_at is set when an attempt
-- ends; error IS NULL marks a successful run, which is never run again.
CREATE TABLE public.scheduler_runs (
    job           TEXT        NOT NULL,
    business_date DATE        NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL,
    finished_at   TIMESTAMPTZ,
    attempt       INT         NOT NULL,
    failures      INT         NOT NULL DEFAULT 0,
    error         TEXT,
    PRIMARY KEY (job, business_date)
);
