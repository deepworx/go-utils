# Audit: CPQ-specific assumptions in the event-sourcing reference code

Ticket: DWX-206 (child of map DWX-205, "Postgres-only event sourcing library in go-utils").

Source: ariadne-cpq PR 230, read from a shallow clone of the PR branch. All paths below are
relative to `services/api/` unless noted, and `file:line` refers to that branch. ADRs referenced
are `docs/adr/0053`-`0059` of ariadne-cpq.

Question: which assumptions in the reference code are CPQ-specific and would have to be
generalized for a reusable library? For each package the table reads
"assumption -> where -> what a library needs instead".

## Summary: the biggest generalization issues

1. **Everything assumes a single tenant.** No table has a tenant column (`events`,
   `consumer_cursors`, `guards`, `identifier_sequences`, `parked_events`, `scheduler_runs`,
   `webhook_inbox`; `db/migration/000001_init.up.sql:28-520`). Nothing sets a session variable
   or uses RLS, and the pool is shared. The worst cases are the **side tables written in the
   command transaction**. `guards` has PK `(rule, key)` (`:125-131`), so the same external key
   in two tenants collides. `identifier_sequences` has PK `(context, name)` (`:106-112`), so
   every tenant would draw from one gapless range. The `changes` notification payload is
   `<entity>:<id>` (`lib/projection/projection.go:281`), so every SSE subscriber on every pod
   gets every tenant's invalidations. The scheduler has one business timezone for all jobs
   (`lib/scheduler/scheduler.go:179`) and one run row per `(job, business_date)`. The library
   needs a tenant model up front: a `tenant_id` column with composite keys on the side tables,
   a tenant in `Envelope`/metadata, and tenant-aware notifications. Optionally RLS through
   `SET LOCAL`. This is a design decision for the map, not a mechanical rename.
2. **sqlc coupling sits in the adapters, not in `lib`.** `lib/*` uses hand-written SQL strings.
   The adapters that touch `events` (`PgEventLog`, `PgSource`), plus `readwait`, `guard`,
   `numbering` and `consumermetrics`, all import the CPQ sqlc package
   `internal/infrastructure/postgres`. A library must own its SQL: either hand-written
   statements (as `lib/` already does) or sqlc code generated inside the library module. It
   also needs its own embedded migration for the tables it uses.
3. **Protobuf coupling is confined to the concrete codec**
   (`internal/infrastructure/eventstore/codec.go`). The machinery's `lib/eventstore.Codec` is
   already format-neutral (`Encode(any)`/`Decode(...) any`, `lib/eventstore/codec.go:13-22`).
   The library should ship the neutral interface plus the registry and upcaster chain, with
   generic type registration that does not depend on `shared.Event`. A protobuf codec, if
   offered at all, belongs in an opt-in subpackage so `google.golang.org/protobuf` is not
   forced on users. A JSON codec is a sensible default. The `BYTEA` column can stay.
4. **There is no Prometheus coupling.** The only metrics seam in `lib` is the one-method
   `consumer.Metrics` interface (`lib/consumer/runner.go:89-98`). Its only implementation
   is not a metric. `internal/infrastructure/consumermetrics/metrics.go:37-49` persists the
   stopped flag to `consumer_cursors.stopped` through sqlc. `readwait` and the gauges then read
   that flag. Gauges are **OpenTelemetry**, defined in internal code with `cpq.*` names
   (`internal/infrastructure/telemetry/gauges.go:19-28`). A library should make "stopped" part
   of the cursor store (`Cursors.MarkStopped`), because it is state that correctness
   (read-your-writes) depends on. It can expose OTel gauges as an optional observer with
   neutral names.
5. **The reference code depends on CPQ-local forks of go-utils packages that have diverged.**
   `lib/postgres` has a stream-locking `UnitOfWork` (`Execute/Within/Transaction`, tx carried
   in the context, `lib/postgres/unitofwork.go:57-80`). go-utils `pkg/postgres` has
   `Execute(ctx, fn(ctx, Transaction))` with no tx in the context and no stream lock.
   `lib/ctxutil` has `WithUserID/CorrelationID/CausationID`. go-utils `pkg/ctxutil` has
   `RequestID` plus `Claims{UserID, TenantID, ...}` and no correlation or causation IDs.
   `lib/postgres.NewPool` requires the CPQ `lib/shutdown.Registry`
   (`lib/postgres/postgres.go:67-73`). The library has to choose: extend `pkg/postgres` and
   `pkg/ctxutil`, or keep its own transaction-in-context UoW and a metadata-provider hook
   inside the event-store package.
6. **The `stream_type` model is generic, but stream IDs must be UUIDs.** `stream_type` is a free
   `TEXT` with CPQ example values. Every API takes `uuid.UUID` stream IDs
   (`Envelope.StreamID`, `UnitOfWork.Execute(ctx, stream uuid.UUID, ...)`,
   `Repository.Load(id uuid.UUID)`). That is acceptable for a library, but it must be stated.
   Combined with a tenant model, the stream identity is either `(tenant, uuid)` or a globally
   unique UUID.
7. **Hard-coded names everywhere.** Tables `events`, `consumer_cursors`, `parked_events`,
   `scheduler_runs`, `webhook_inbox`, `guards`, `identifier_sequences`. Channels `events`,
   `webhook_inbox`, `changes`. Advisory-lock classes `0x434F0001` (consumers) and
   `0x5C4E0001/2` (scheduler). The single-bigint advisory space is used for stream locks. A
   library needs a configurable schema or table prefix (at least a schema name) and
   configurable channel names. It also has to document which advisory-lock spaces it owns,
   because a host app using `pg_advisory_xact_lock(bigint)` itself would collide with stream
   locks.
8. **Two bugs or stale spots worth fixing before porting.**
   - `consumer.Runner.wait` calls `r.waker.Wake()` on every idle loop
     (`lib/consumer/runner.go:365-368`). `PgWaker.Wake` appends a new channel each call and
     never removes it (`lib/consumer/postgres.go:184-191`). The subscriber list grows
     unboundedly, and `signal()` iterates all of them. `ManualWaker` in tests returns one
     shared channel, which hides this. Wake should be registered once per consumer.
   - `EventLog.ListByEventTypes` pages on `global_position` alone
     (`db/queries/eventstore.sql:109-131`). ADR-0053 and the code's own comments say that
     column is not a safe order. Drop it, because the consumer cursor replaces it. The
     `Store.Append` doc comment still says it "projects the synchronous read models and
     enqueues the outbox jobs" (`internal/application/eventstore/store.go:71-73`). It does
     neither.

## Per-package audit

### `lib/eventstore` (Envelope, Cursor, Codec)

| Assumption | Where | Library needs instead |
|---|---|---|
| Envelope has fixed metadata (correlation, causation, occurred_by) and no tenant or extensible metadata | `lib/eventstore/envelope.go:41-56` | `TenantID` (if multi-tenant is in scope) and possibly a `Metadata map`/JSONB column for app-specific headers |
| Stream IDs are `uuid.UUID` | `envelope.go:44` | Keep, but document it, or make the ID generic |
| `Cursor` is duplicated in the application layer | `envelope.go:23-34` vs `internal/application/eventstore/port.go:58-69` | One `Cursor` type in the library |
| Codec is format-neutral (`any`) because `lib` may not import `internal/domain` | `lib/eventstore/codec.go:13-22` | Keep the neutral interface. With generics in the library, a typed `Codec[E]` becomes possible |

Tables needed: none directly (types only).

### `internal/infrastructure/eventstore` (PgEventLog, PgSource, Codec)

| Assumption | Where | Library needs instead |
|---|---|---|
| All SQL comes from the CPQ sqlc package | `pg_log.go:17,43-49`, `pg_source.go:12,132-137` | SQL owned by the library (hand-written or sqlc generated inside it) |
| Unique violation maps to the CPQ domain error `shared.ErrConcurrencyConflict` (identifier `CPQ.COMMON.CONCURRENCY_CONFLICT`) | `pg_log.go:106-108`, `internal/domain/shared/errors.go:36` | Library sentinel `eventstore.ErrConcurrencyConflict`, matched with `errors.Is` |
| `PgEventLog` speaks `appeventstore.StoredEvent`/`Cursor`, `PgSource` speaks `libes.Envelope`: two adapters over one table | `pg_log.go:25-33`, `pg_source.go:17-23` | One store type with one row type |
| A causation ID that is not a UUID is silently dropped | `pg_log.go:142-153` | Validate on append and return an error |
| Codec is protobuf-only: `Mapper[E shared.Event, M proto.Message]`, `UpcastMessage`, deterministic proto marshal, `DecodeMessage` for the CPQ `events dump` CLI | `codec.go:6,19-48,257-267,222-228` | Generic registry and upcaster chain keyed on `(event_type, schema_version)` with a pluggable serializer. JSON by default, protobuf as an opt-in subpackage |
| Event type and version come from the domain `shared.Event` interface (`EventType()`, `SchemaVersion()`) | `codec.go:90-92`, `internal/domain/shared/event.go:7-10` | A library-level `Event` interface with the same two methods, or registration by explicit name and version |
| Registration errors panic at wiring time | `codec.go:94-103` | Fine for a library (it mirrors `http.Handle`), but document it |
| `Head` uses `max(transaction_id)`/`max(global_position)` without the xmin bound, so in-flight writes below the head can be skipped by a `StartAtNow` seed | `pg_source.go:86-99`, `db/queries/eventstore.sql:181-184` | Bound the head by `pg_snapshot_xmin` as well, or document the gap |

Tables needed: `events` with its triggers `events_immutable` (function `prevent_event_mutation`)
and `events_notify` (function `notify_events`, channel `events`, per statement), plus indexes
`events_cursor (transaction_id, global_position)` and `idx_events_type_position`
(`000001_init.up.sql:28-94`). Postgres 13 or later is required for `xid8`,
`pg_current_xact_id` and `pg_snapshot_xmin`.

### `internal/application/eventstore` (Store, Repository, ports) and `domain/shared/aggregate.go`

| Assumption | Where | Library needs instead |
|---|---|---|
| Actor, correlation and causation are read from the CPQ `lib/ctxutil` (`UserID`, `CorrelationID`, `CausationID`) | `store.go:90-95` | A metadata-provider hook (`func(ctx) Metadata`) defaulting to go-utils `ctxutil`. go-utils lacks correlation and causation today and derives the user from `Claims` |
| No tenant is recorded on append | `store.go:103-115` | Tenant from the context or claims, if multi-tenant is in scope |
| Event IDs are UUID v7 and `occurred_on` is forced to UTC | `store.go:84-88,104` | Fine as defaults. Allow an injectable clock and ID generator for tests |
| Errors are CPQ domain errors (`shared.ErrAggregateNotFound`, `ErrConcurrencyConflict`) | `repository.go:54,104,112`, `pg_log.go:107` | Library sentinels |
| `Replayable` requires `AggregateUUID()`, `StreamType()`, `ApplyEvent(shared.Event)` | `repository.go:18-27` | The same contract with a library `Event` type. Generic `Repository[T]` ports almost as is |
| `AggregateBase[ID AggregateID]` with `AggregateID{Type(); ID() uuid.UUID}` | `internal/domain/shared/aggregate.go:13-66`, `shared/id.go:8-11` | Generic, no CPQ content. Port it as is |
| `ListByEventTypes` pages by `global_position` | `port.go:123-127` | Drop it (see summary item 8) |
| CPQ requirement IDs in docs (`CPQ-ES-4`, ADR-0019 "catalog pin") | `repository.go:78,118`, `port.go:114-115` | Remove |
| `PermanentReactionError` is the CPQ marker that the wiring maps to `processmanager.Park` | `reaction_error.go:11-29` | Move into `processmanager` as a library sentinel or wrapper with a default `Classifier` honoring it |
| `UnitOfWork` is aliased to `lib/postgres.UnitOfWork` | `port.go:136` | See `lib/postgres` below |

Tables needed: `events` (through `EventLog`).

### `lib/postgres` (pool, stream-locking UnitOfWork)

| Assumption | Where | Library needs instead |
|---|---|---|
| CPQ fork of go-utils `pkg/postgres`. `NewPool` requires the CPQ `lib/shutdown.Registry`. The doc comment claims OTel tracing and pool metrics, but the code does neither | `postgres.go:16,64-106` | Use go-utils `pkg/postgres.NewPool` (already has OTel metrics) and drop the fork |
| `UnitOfWork` carries the tx in the context (`WithTxContext`/`TxFromContext`) and has `Execute(stream)`/`Within(stream)`/`Transaction`. go-utils' UoW passes a `Transaction` argument | `unitofwork.go:14-80` | Decide: extend `pkg/postgres` with a tx-in-context mode, or keep a stream UoW inside the event-store package. Every other package (`consumer`, `projection`, `webhookinbox`, `guard`, `numbering`) depends on `TxFromContext` |
| One command transaction changes exactly one stream (ADR-0053). The stream is fixed at open time | `unitofwork.go:41-56,108-122` | A general library rule, not CPQ-specific. Keep it and document it |
| Stream lock is `pg_advisory_xact_lock(fnv64a(uuid))` in the **single-bigint** advisory space | `unitofwork.go:138,160-164` | Document the owned lock space, or move to the classed two-int space with a configurable class so it cannot collide with host-app locks. With tenants, hash `(tenant, stream)` |

Tables needed: none. Session-level advisory locks only.

### `lib/consumer`

| Assumption | Where | Library needs instead |
|---|---|---|
| Hard-coded table `consumer_cursors(consumer, last_xid, last_position, updated_at)` | `postgres.go:35,50-55,69` | Configurable schema or table name |
| Cursor key is the consumer name only, and one consumer reads all streams of all tenants | `postgres.go:31-46`, `consumer.go:70-92` | Fine for a global consumer. If per-tenant consumers or rebuilds are required, key on `(consumer, tenant)` and filter `ReadAfter` by tenant |
| The `stopped`, `stopped_at`, `stopped_error` columns belong to this table but are written by internal `consumermetrics` through sqlc and read by `readwait` | `000001_init.up.sql:153-155`, `internal/infrastructure/consumermetrics/metrics.go:43` | `PgCursors.MarkStopped`/`Stopped` owned by the package |
| `Metrics` interface is a single `Stopped(consumer, bool)`. It is misnamed, because it is really state persistence | `runner.go:89-98` | Split it into persisted state (above) and an optional OTel observer |
| Claims use `pg_try_advisory_lock(0x434F0001, fnv32(name))` and hold one pooled connection per claimed consumer for the pod's lifetime | `postgres.go:101,117-142` | Configurable class. Document the pool sizing (N consumers means N held connections, plus one for each listener) |
| `PgWaker` takes the channel as a parameter, but the trigger hard-codes `'events'`, and CPQ wires the constant in `cmd/server/app_runtime.go:40` | `postgres.go:175`, `000001_init.up.sql:86` | Channel name comes from config, and the migration uses the same value |
| **Bug:** `Runner.wait` calls `Wake()` on every idle loop and `PgWaker.Wake` appends forever | `runner.go:365-368`, `postgres.go:184-191` | Register once per consumer and reuse the channel |
| `XminAge` reads `pg_stat_activity`, which needs `pg_read_all_stats` or the same role to see other sessions | `postgres.go:252-265` | Document the privilege |
| Doc mentions "the ordered Accounting consumer" and the webhook inbox | `consumer.go:3-5,27-29` | Remove the CPQ references |

Tables needed: `consumer_cursors`. Reads `events` through `Source`. Listens on the `events`
channel.

### `lib/projection`

| Assumption | Where | Library needs instead |
|---|---|---|
| Hard-coded notify channel `ChangeChannel = "changes"`. Payload is `<header table>:<uuid>`, with no tenant | `projection.go:261-285` | Configurable channel. Tenant in the payload, or a per-tenant channel, so subscribers can filter |
| Version naming `<table>_v<N>`, `<table>_v<N>_rebuild`, reader view `<table>`, consumer name `<name>@v<N>` | `projection.go:134-148,288`, `swap.go:21-75` | Generic. Keep it, but make it a documented convention |
| Runtime DDL (`CREATE TABLE ... LIKE ... INCLUDING ALL`, `DROP TABLE`, `CREATE VIEW`) needs DDL privileges for the app role | `swap.go:21-56,64-75` | Document it (conflicts with least privilege), or run rebuilds through a separate privileged role or CLI |
| Header and child model: header upsert on `Key`, children deleted and reinserted by the `Owner` UUID. Not tenant-scoped | `projection.go:39-50,231-259,320-322` | Fine if owner UUIDs are global. Otherwise add the tenant to the key and the owner delete |
| `Load` returns `any`. `Meta` comment explains a quote-specific concern | `projection.go:69-72,111-114` | Use generics (`Detail[S]`). Strip the CPQ wording |
| Read-model tables are app-owned (the `quote_v1` family) | `000001_init.up.sql:171-365` | The library ships no read-model DDL, only the conventions |

Tables needed: `consumer_cursors` (via consumer) and the app's `<table>_v<N>` tables and views.
Notifies on `changes`.

### `lib/processmanager`

| Assumption | Where | Library needs instead |
|---|---|---|
| Hard-coded table `parked_events(consumer, stream_id, stream_type, event_id, stream_position, transaction_id, global_position, event_type, error, parked_at)`, PK `(consumer, event_id)` | `postgres.go:26,39-47`, `000001_init.up.sql:376-401` | Configurable name. Tenant column if the Ops view must be tenant-scoped |
| Actor is `system:<reactor>`, written with CPQ `lib/ctxutil.WithUserID/WithCausationID/WithCorrelationID` | `processmanager.go:288-295` | The same metadata hook as `Store`. go-utils `ctxutil` has no correlation or causation IDs, and its user comes from `Claims` |
| The reaction context does not carry a tenant | `processmanager.go:288-295` | Propagate the event's tenant into the context, or a reaction command lands in no tenant |
| Default `Classifier` is "Retry everything". The permanent marker lives in the internal application layer | `processmanager.go:152-155`, `internal/application/eventstore/reaction_error.go` | Ship the marker in the library |
| Doc references River, quote owners and emails | `processmanager.go:45-48`, `seed.go:17-22` | Remove |
| `PgParks.Counts` is the gauge source, consumed by internal OTel code | `postgres.go:92-111` | Keep. Optional OTel observer in the library |

Tables needed: `parked_events`, plus `consumer_cursors` and `events` (via consumer, `Seed`
and `Redrive.ReadOne`).

### `lib/scheduler`

| Assumption | Where | Library needs instead |
|---|---|---|
| One business timezone per scheduler (`loc`). CPQ default is `Europe/Berlin` | `scheduler.go:178-187`, `internal/infrastructure/config/config.go:207-209`, `cmd/server/main.go:35`, schema comment `000001_init.up.sql:420` | Already parameterized. Multi-tenant would need a per-tenant zone, with the run key `(job, tenant, business_date)` or the job iterating tenants itself |
| Hard-coded table `scheduler_runs(job, business_date, started_at, finished_at, attempt, failures, error)` | `postgres.go:104-153`, `000001_init.up.sql:418-446` | Configurable name |
| Lock classes `0x5C4E0001` (leader) and `0x5C4E0002` (jobs). The comment says `"SCHE"`, but `0x5C4E` is not that ASCII | `postgres.go:21-24` | Configurable classes |
| Due-ness is derived from aggregate state, and jobs are idempotent per date (ADR-0055) | `scheduler.go:1-13` | Generic policy. Keep it |

Tables needed: `scheduler_runs`. No events dependency at all, so this is a standalone package.

### `lib/webhookinbox`

| Assumption | Where | Library needs instead |
|---|---|---|
| Hard-coded table `webhook_inbox`, with `UNIQUE (provider, event_id)` and status CHECK | `postgres.go:37-200`, `000001_init.up.sql:462-507` | Configurable name. Add a tenant column to the unique key if provider accounts are per tenant |
| Payload column is `JSONB`, and an empty payload is coerced to `{}` | `postgres.go:31-34`, schema `:475` | Non-JSON providers (form-encoded, XML) fail. Use `BYTEA` plus a content type, or document the restriction |
| Trigger `webhook_inbox_notify` (function `notify_webhook_inbox`) sends `NEW.provider` on channel `webhook_inbox` | `000001_init.up.sql:511-520`, `cmd/server/app_runtime.go:45` | Configurable channel |
| `ClaimName` prefix `webhook_inbox:` in the consumer claim space | `webhookinbox.go:294` | Fine |
| PandaDoc references in docs | `webhookinbox.go:103-105`, schema `:466-469` | Remove |
| Not event sourcing: it is a leased work queue | `webhookinbox.go:1-27` | Make it a sibling package (e.g. `pkg/inbox`), decoupled from the event store |

Tables needed: `webhook_inbox` and its trigger. Uses `pgnotify` and `consumer.Claims`.

### `lib/pgnotify`

| Assumption | Where | Library needs instead |
|---|---|---|
| Generic one-channel LISTEN fan-out. Channel is a parameter | `pgnotify.go:63-70` | Port as is. It duplicates `PgWaker`'s listen loop, so the two should share one listener implementation |
| Drops payloads for slow subscribers. A comment ties `Close` ordering to the CPQ shutdown registry | `pgnotify.go:141-170` | Document it. Use go-utils `pkg/shutdown` |
| Holds one pooled connection for its lifetime and does not work behind transaction-mode PgBouncer | `pgnotify.go:91-109` | Document it |

Tables needed: none.

### `lib/backoff`

Generic: cancellable `Sleep`, `Exponential`, `Jitter` (`lib/backoff/backoff.go:15-56`). No CPQ
assumptions. Port it as is, as an internal or exported helper.

### `internal/infrastructure/readwait`

| Assumption | Where | Library needs instead |
|---|---|---|
| Reads `consumer_cursors` through the CPQ sqlc `ReadConsumerCursor` (including `stopped`) | `readwait.go:21,173-191`, `db/queries/consumer_cursors.sql:1-6` | Use the consumer package's `Cursors` (with stopped state) |
| 2 s default bound from ADR-0056 and a 25 ms poll | `readwait.go:26-31` | Fine as defaults |
| Poller goroutine runs on `context.Background()` | `readwait.go:132-134` | Acceptable (it stops with the last waiter). Tie it to a gate lifecycle for clean shutdown |

Tables needed: `consumer_cursors`.

### `internal/infrastructure/guard`

| Assumption | Where | Library needs instead |
|---|---|---|
| CPQ rule `ActiveDealQuote = "quote.active_deal"` and `DealGuard` (HubSpot deal, domain error) | `guard.go:31-33`, `deal.go:1-42` | Stays in the app. The library ships only `Guards` |
| CPQ sqlc queries `ClaimGuard/ReleaseGuard/GuardOwner/ListGuards/DeleteGuardsByRule` | `guard.go:23,57-133`, `db/queries/numbering.sql:14-34` | SQL owned by the library |
| PK `(rule, key)` is global, so the same key collides across tenants | `000001_init.up.sql:125-133` | PK `(tenant, rule, key)` |
| Must run in the command transaction (`TxFromContext`) | `guard.go:52-55` | Same tx-in-context dependency as `lib/postgres` |

Tables needed: `guards` with index `idx_guards_owner`.

### `internal/infrastructure/numbering`

| Assumption | Where | Library needs instead |
|---|---|---|
| CPQ range `("quoting", "quote_number")` and format `Q-%06d` | `numbering/quote.go:18-21,58` | Stays in the app. The library offers `Draw(ctx, scope, name) (int64, error)` |
| CPQ sqlc `DrawIdentifier` (upsert-increment, row locked until commit) | `quote.go:51-54`, `db/queries/numbering.sql:1-12` | SQL owned by the library |
| PK `(context, name)` is global, so all tenants share one gapless range | `000001_init.up.sql:106-112` | PK `(tenant, context, name)` |

Tables needed: `identifier_sequences`.

## Table, trigger and function inventory per package

| Package | Tables | Triggers / functions | NOTIFY channels | Advisory locks |
|---|---|---|---|---|
| event store (log, source, store) | `events` | `events_immutable` / `prevent_event_mutation()`, `events_notify` / `notify_events()` | sends `events` (per statement, empty payload) | `pg_advisory_xact_lock(bigint)` per stream |
| consumer | `consumer_cursors` (incl. `stopped*` columns) | none | listens on `events` | session `(0x434F0001, fnv32(name))` |
| projection | app tables `<t>_v<N>`, views `<t>` | none (runtime DDL) | sends `changes` | via consumer |
| processmanager | `parked_events` | none | none | via consumer |
| scheduler | `scheduler_runs` | none | none | session `(0x5C4E0001, 0)`, `(0x5C4E0002, fnv32(job))` |
| webhookinbox | `webhook_inbox` | `webhook_inbox_notify` / `notify_webhook_inbox()` | sends and listens on `webhook_inbox` (payload provider) | via consumer claims, name `webhook_inbox:<provider>` |
| pgnotify | none | none | any (parameter) | none |
| readwait | `consumer_cursors` (read) | none | none | none |
| guard | `guards` | none | none | none |
| numbering | `identifier_sequences` | none | none | none |
| backoff | none | none | none | none |

Server-side requirements: PostgreSQL 13 or later (`xid8`, `pg_current_xact_id`,
`pg_snapshot_xmin(pg_current_snapshot())`) and plpgsql. Session connections are needed for
LISTEN and session advisory locks, so transaction-mode PgBouncer will not work. DDL rights are
needed for projection rebuilds, and `pg_stat_activity` visibility for `XminAge`.

## Dependency hygiene

- `lib/*` imports nothing from `internal/` or `gen/`. This is enforced by ADR-0059. The
  CPQ-specific coupling sits entirely in the internal adapters listed above.
- `lib/*` imports CPQ-local forks: `lib/postgres` (consumer, projection, webhookinbox),
  `lib/ctxutil` (processmanager) and `lib/shutdown` (lib/postgres). All three must be
  replaced by go-utils `pkg/postgres`, `pkg/ctxutil` and `pkg/shutdown`, or by library-owned
  equivalents, and their APIs differ (summary item 5).
- No Prometheus import anywhere in scope. OTel is used only in
  `internal/infrastructure/telemetry`.
