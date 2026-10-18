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
-- pkg/eventsourcing/eventstore: the append-only event log. Events are
-- ordered by (transaction_id, global_position); consumers only read below
-- pg_snapshot_xmin. recorded_at is set by the store clock.
-- ---------------------------------------------------------------------------
CREATE TABLE public.events (
    global_position BIGINT      GENERATED ALWAYS AS IDENTITY,
    transaction_id  XID8        NOT NULL DEFAULT pg_current_xact_id(),
    event_id        UUID        PRIMARY KEY,
    tenant_id       TEXT        NOT NULL,
    stream_type     TEXT        NOT NULL,
    stream_id       UUID        NOT NULL,
    stream_position INT         NOT NULL CHECK (stream_position > 0),
    event_type      TEXT        NOT NULL,
    schema_version  INT         NOT NULL CHECK (schema_version > 0),
    event_data      BYTEA       NOT NULL,
    actor           TEXT        NOT NULL,
    correlation_id  TEXT        NOT NULL,
    causation_id    UUID        NULL,
    trace_context   TEXT        NULL,
    recorded_at     TIMESTAMPTZ NOT NULL,
    -- Required under this exact name: the store maps its violation to
    -- ErrConcurrencyConflict, and it is the last line against duplicate
    -- stream positions (ADR-0003, ADR-0013).
    CONSTRAINT events_stream_position_key UNIQUE (tenant_id, stream_id, stream_position)
);

-- Consumer reads in log order.
CREATE INDEX events_log_idx
    ON public.events (transaction_id, global_position);

-- Consumer reads filtered by event type.
CREATE INDEX events_event_type_idx
    ON public.events (event_type, transaction_id, global_position);

-- The log is append-only (ADR-0009).
CREATE FUNCTION public.events_reject_change() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    RAISE EXCEPTION '%.events is append-only: % rejected', TG_TABLE_SCHEMA, TG_OP
        USING ERRCODE = 'restrict_violation';
END
$$;

CREATE TRIGGER events_immutable
    BEFORE UPDATE OR DELETE ON public.events
    FOR EACH ROW EXECUTE FUNCTION public.events_reject_change();

CREATE TRIGGER events_no_truncate
    BEFORE TRUNCATE ON public.events
    FOR EACH STATEMENT EXECUTE FUNCTION public.events_reject_change();

-- Wakes the consumers on channel <schema>.events, without payload.
CREATE FUNCTION public.events_notify() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    PERFORM pg_notify(TG_TABLE_SCHEMA || '.events', '');
    RETURN NULL;
END
$$;

CREATE TRIGGER events_notify
    AFTER INSERT ON public.events
    FOR EACH STATEMENT EXECUTE FUNCTION public.events_notify();

-- ---------------------------------------------------------------------------
-- pkg/eventsourcing/eventstore: Command IDs of deduplicated commands
-- (ADR-0012), written in the Command transaction. A row holds the Stream
-- the command changed and its result: the Cursor of its last event (0, 0
-- when it raised none) and the Aggregate version after it. recorded_at is
-- set by the store clock; eventstore.CommandCleanupJob deletes old rows.
-- ---------------------------------------------------------------------------
CREATE TABLE public.processed_commands (
    tenant_id      TEXT        NOT NULL,
    command_id     UUID        NOT NULL,
    stream_id      UUID        NOT NULL,
    transaction_id XID8        NOT NULL,
    position       BIGINT      NOT NULL,
    version        INT         NOT NULL,
    recorded_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, command_id)
);

-- Cleanup scan by age.
CREATE INDEX processed_commands_recorded_at_idx
    ON public.processed_commands (recorded_at);

-- ---------------------------------------------------------------------------
-- pkg/eventsourcing/consumer: one Cursor per Consumer. The Cursor is the
-- last processed matching event; a filtered Cursor stays at its last
-- matching event. Updates are compare-and-set on the expected Cursor, which
-- fences a former lease holder. An empty event_types array means every
-- type. stopped stays set until Resume.
-- ---------------------------------------------------------------------------
CREATE TABLE public.consumer_cursors (
    name            TEXT        PRIMARY KEY,
    transaction_id  XID8        NOT NULL DEFAULT '0',
    global_position BIGINT      NOT NULL DEFAULT 0,
    event_types     TEXT[]      NOT NULL,
    stopped         BOOLEAN     NOT NULL DEFAULT false,
    stopped_at      TIMESTAMPTZ NULL,
    stopped_error   TEXT        NULL
);

-- consumer_pending counts the visible events after a Consumer's Cursor that
-- match its event types, up to cap+1. Use it to guard a migration that
-- switches a view to a new Projection version. It raises when the Consumer
-- has no Cursor or is Stopped. Each event type is counted on its own index
-- range.
CREATE FUNCTION public.consumer_pending(consumer_name TEXT, cap INT) RETURNS INT
    LANGUAGE plpgsql STABLE AS
$$
DECLARE
    c       public.consumer_cursors;
    horizon XID8 := pg_snapshot_xmin(pg_current_snapshot());
    total   BIGINT;
BEGIN
    SELECT * INTO c FROM public.consumer_cursors WHERE name = consumer_name;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'consumer % has no cursor', consumer_name
            USING ERRCODE = 'no_data_found';
    END IF;
    IF c.stopped THEN
        RAISE EXCEPTION 'consumer % is stopped: %', consumer_name, c.stopped_error
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;

    IF cardinality(c.event_types) = 0 THEN
        SELECT count(*) INTO total FROM (
            SELECT 1 FROM public.events e
            WHERE (e.transaction_id, e.global_position) > (c.transaction_id, c.global_position)
              AND e.transaction_id < horizon
            ORDER BY e.transaction_id, e.global_position
            LIMIT cap + 1
        ) pending;
    ELSE
        SELECT COALESCE(sum(per_type.n), 0) INTO total
        FROM unnest(c.event_types) AS t(event_type)
        CROSS JOIN LATERAL (
            SELECT count(*) AS n FROM (
                SELECT 1 FROM public.events e
                WHERE e.event_type = t.event_type
                  AND (e.transaction_id, e.global_position) > (c.transaction_id, c.global_position)
                  AND e.transaction_id < horizon
                ORDER BY e.transaction_id, e.global_position
                LIMIT cap + 1
            ) pending
        ) per_type;
    END IF;
    RETURN least(total, cap + 1);
END
$$;

-- ---------------------------------------------------------------------------
-- pkg/eventsourcing/reactor: Parked Streams. A parked_streams row is the
-- hold of one Stream of a Reactor (consumer); the live handler, Redrive and
-- Discard lock it, so they serialize per (consumer, tenant_id, stream_id).
-- parked_events are the held events in stream_position order: the failed
-- one has a cause, the ones held behind it have none. A released event is
-- deleted; a discarded one stays with discarded_at and discarded_by, and
-- the service owns its cleanup.
-- The hold carries the auto-redrive schedule: redrives counts the failed
-- auto-redrives, next_attempt_at is the next one (NULL when auto-redrive is
-- off) and exhausted marks a spent schedule.
-- A redrive locks the hold only to claim its head and to settle the
-- outcome, never while it reacts: redrive_claim is the token of the redrive
-- replaying the head and redrive_claimed_until its expiry, after which
-- another redrive may take the head over.
-- ---------------------------------------------------------------------------
CREATE TABLE public.parked_streams (
    consumer        TEXT        NOT NULL,
    tenant_id       TEXT        NOT NULL,
    stream_id       UUID        NOT NULL,
    parked_at       TIMESTAMPTZ NOT NULL,
    redrives        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NULL,
    exhausted       BOOLEAN     NOT NULL DEFAULT false,
    redrive_claim         UUID        NULL,
    redrive_claimed_until TIMESTAMPTZ NULL,
    PRIMARY KEY (consumer, tenant_id, stream_id)
);

-- The auto-redrives that come due.
CREATE INDEX parked_streams_due_idx
    ON public.parked_streams (consumer, next_attempt_at)
    WHERE NOT exhausted;

CREATE TABLE public.parked_events (
    consumer        TEXT        NOT NULL,
    tenant_id       TEXT        NOT NULL,
    stream_id       UUID        NOT NULL,
    stream_type     TEXT        NOT NULL,
    stream_position INT         NOT NULL,
    event_id        UUID        NOT NULL,
    event_type      TEXT        NOT NULL,
    cause           TEXT        NULL,
    attempts        INT         NOT NULL DEFAULT 0,
    parked_at       TIMESTAMPTZ NOT NULL,
    discarded_at    TIMESTAMPTZ NULL,
    discarded_by    TEXT        NULL,
    PRIMARY KEY (consumer, event_id)
);

-- The held events of one Stream in order.
CREATE INDEX parked_events_held_idx
    ON public.parked_events (consumer, tenant_id, stream_id, stream_position)
    WHERE discarded_at IS NULL;

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
-- pkg/scheduler: one row per interval job. due_at is the next instant the job
-- is due: the next grid point, or the retry time of an aborted attempt.
-- finished_at IS NULL marks a running attempt; error IS NULL marks a
-- successful one. last_success_at is the finish of the latest successful
-- attempt and survives failed ones.
-- ---------------------------------------------------------------------------
CREATE TABLE public.scheduler_intervals (
    job             TEXT        PRIMARY KEY,
    due_at          TIMESTAMPTZ NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    finished_at     TIMESTAMPTZ,
    attempt         INT         NOT NULL,
    failures        INT         NOT NULL DEFAULT 0,
    error           TEXT,
    last_success_at TIMESTAMPTZ
);

-- ---------------------------------------------------------------------------
-- pkg/inbox: deduplicated work queue of incoming external messages, one
-- row per (source, tenant_id, key). Parked rows are kept until an operator
-- acts.
-- ---------------------------------------------------------------------------
CREATE TABLE public.inbox_messages (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source           TEXT        NOT NULL,
    key              TEXT        NOT NULL,
    type             TEXT        NOT NULL,
    tenant_id        TEXT        NOT NULL DEFAULT '',
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
    -- Required under this exact name: Insert deduplicates on it. An empty
    -- tenant_id is a message without a Tenant (ADR-0003, ADR-0015).
    CONSTRAINT inbox_messages_source_tenant_key_key UNIQUE (source, tenant_id, key)
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
    PERFORM pg_notify(TG_TABLE_SCHEMA || '.inbox', NEW.source);
    RETURN NULL;
END
$$;

CREATE TRIGGER inbox_messages_notify
    AFTER INSERT ON public.inbox_messages
    FOR EACH ROW EXECUTE FUNCTION public.inbox_messages_notify();
