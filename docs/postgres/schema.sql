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

-- ---------------------------------------------------------------------------
-- pkg/postgres/guard: tenant-scoped uniqueness rules.
-- ---------------------------------------------------------------------------
CREATE TABLE public.guards (
    tenant_id  TEXT        NOT NULL,
    rule       TEXT        NOT NULL,
    key        TEXT        NOT NULL,
    claimed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, rule, key)
);

-- ---------------------------------------------------------------------------
-- pkg/postgres/numbering: tenant-scoped gapless numbers.
-- ---------------------------------------------------------------------------
CREATE TABLE public.identifier_sequences (
    tenant_id  TEXT   NOT NULL,
    context    TEXT   NOT NULL,
    name       TEXT   NOT NULL,
    last_value BIGINT NOT NULL CHECK (last_value > 0),
    PRIMARY KEY (tenant_id, context, name)
);

-- ---------------------------------------------------------------------------
-- pkg/scheduler: one row per daily job and Business date. finished_at is set
-- when an attempt ends; error IS NULL marks a successful run, which is never
-- run again.
-- ---------------------------------------------------------------------------
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

-- ---------------------------------------------------------------------------
-- pkg/inbox: deduplicated work queue of incoming external messages, one
-- row per (source, key). Parked rows are kept until an operator acts.
-- ---------------------------------------------------------------------------
CREATE TABLE public.inbox_messages (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source           TEXT        NOT NULL,
    key              TEXT        NOT NULL,
    type             TEXT        NOT NULL,
    tenant_id        TEXT        NULL,
    payload          BYTEA       NOT NULL,
    status           TEXT        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'done', 'ignored', 'parked', 'discarded')),
    attempts         INT         NOT NULL DEFAULT 0,
    claim_generation BIGINT      NOT NULL DEFAULT 0,
    claimed_until    TIMESTAMPTZ NULL,
    next_attempt_at  TIMESTAMPTZ NOT NULL,
    received_at      TIMESTAMPTZ NOT NULL,
    settled_at       TIMESTAMPTZ NULL,
    error            TEXT        NULL,
    trace_context    TEXT        NULL,
    discarded_at     TIMESTAMPTZ NULL,
    discarded_by     TEXT        NULL,
    CONSTRAINT inbox_messages_source_key_key UNIQUE (source, key)
);

-- Claim scan of a Worker: due pending rows of one Source.
CREATE INDEX inbox_messages_pending_idx
    ON public.inbox_messages (source, next_attempt_at)
    WHERE status = 'pending';

-- Cleanup scan: settled rows by age. Parked rows are never cleaned up.
CREATE INDEX inbox_messages_settled_idx
    ON public.inbox_messages (settled_at)
    WHERE status IN ('done', 'ignored', 'discarded');

-- Wakes the Workers of the inserted message's Source.
CREATE FUNCTION public.inbox_messages_notify() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    PERFORM pg_notify('public.inbox', NEW.source);
    RETURN NULL;
END
$$;

CREATE TRIGGER inbox_messages_notify
    AFTER INSERT ON public.inbox_messages
    FOR EACH ROW EXECUTE FUNCTION public.inbox_messages_notify();
