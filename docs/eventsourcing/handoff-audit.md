# DWX-205 hand-off audit

Sources: the resolution comments of DWX-206…225 and DWX-227, CONTEXT.md, ADR-0001…0009, the prototype/research branches, and the current `main` code. "Later" means a later comment timestamp.

## 1. Contradictions

| # | Conflict | Authoritative |
|---|---|---|
| C1 | **readwait/Gate.** DWX-208 (`consumer.Gate`), DWX-212 ("Position goes straight to `consumer.Gate`"), DWX-214 ("readwait … read it"), ADR-0005 ("target of the readwait gate") and ADR-0006 ("read-your-write gate") all still describe a gate. DWX-220 replaces it with `(*Consumer).Await` / `p.Await` (active only). | **DWX-220.** Reword ADR-0005/0006. DWX-220 also drops DWX-219's extra condition `min.xid < xmin`; the implementer has to know whether it still applies. |
| C2 | **Filter key.** DWX-219 says the Cursor row stores `stream_types`, and DWX-216 measures guard lag "per stream type". ADR-0006, DWX-220 and DWX-227 filter by **event type**. | **DWX-220/227 + ADR-0006.** The Cursor row stores `event_types`, and `consumer_pending` counts per event type. |
| C3 | **Metrics.** DWX-206 keeps a one-method `Metrics`; DWX-214 removes it (Stopped becomes Cursor state). DWX-217 and the map summary say "every pod reports, `max by`"; DWX-214/ADR-0004 say the lock owner only. | **DWX-214 / ADR-0004.** Fix the DWX-217 line in the map. |
| C4 | **Metric/span names diverge.** DWX-214 has `eventsourcing.parked_event.count`, `inbox.process.duration`, `inbox.message.count`, `inbox.lag`, `scheduler.last_success`, span `process {inbox}`, log key `provider`. DWX-221 has `eventsourcing.reactor.parked.{streams,events,exhausted}` + oldest age. DWX-223 has `inbox.messages.handled`, `inbox.handle.duration`, `inbox.messages`, `inbox.pending.oldest_age`, span `process inbox <source>`, Source. DWX-222 has `scheduler.job.last_success`, `scheduler.run.candidate_failures`, span `scheduler.run <job>`. | The later package tickets (221/222/223). Write one name registry in `internal/telemetry` and rename the log key `provider` to `source`. |
| C5 | **`guard`/`numbering` location.** DWX-208 puts them in `pkg/eventsourcing/*`; DWX-209 puts them in `pkg/postgres/{guard,numbering}`. | **DWX-209** (later; DWX-208 said "moves if…"). |
| C6 | **Inbox model.** DWX-211 has `UNIQUE(provider,event_id)` with the handler resolving the tenant. DWX-213 has table `inbox` with no tenant. DWX-208 says the inbox "uses postgres/lease". DWX-223 has `inbox_messages`, `UNIQUE(source,key)`, nullable `tenant_id`, row lease + `claim_generation`; `lease` is used only for `inbox:telemetry`. | **DWX-223.** |
| C7 | **Schema table list.** DWX-213 lists 7 tables (`inbox`, no `scheduler_intervals`). DWX-222 adds `scheduler_intervals`, and DWX-223 renames to `inbox_messages`. The Cursor table is `consumer_cursors` in DWX-213/214 but `cursors` in DWX-216 (`DELETE FROM cursors`) and DWX-220 (`UPDATE cursors SET stopped`). | Later tickets. Fix the name to `consumer_cursors`. |
| C8 | **Migrate vs docs-only.** DWX-207 recommends River `Migrate`; DWX-213 rejects it. | **DWX-213 / ADR-0003.** DWX-207 was research only. No conflict remains. |
| C9 | **Clock.** DWX-212 `WithClock` vs DWX-215 synctest. These are consistent: `WithClock` is the only clock, and there is no `Clock` interface. Tension: synctest only works in-memory. Reactor `next_attempt_at`, the inbox and the scheduler compute time in Pg (`now()` or Go?), so the Pg tests need `Drain`/`Tick`. | Consistent. State it in the spec. |
| C10 | **aggregate exports.** DWX-212 (and the `prototype/aggregate-api` code) export `Init/UUID/Uncommitted/MarkCommitted/Replay`. DWX-208's note gives `Event` a `SchemaVersion()` and `Repository[T aggregate.Root]`. | **DWX-224** (internal accessor), **DWX-210** (`EventType()` only), **DWX-212** (`Repository[I, A, P]`). The prototype branch is stale. |
| C11 | **Drain semantics.** DWX-215: `ErrRunning` only when this process's runner is active, and it returns "the stopping error". DWX-220: Drain takes the lease itself, gives `ErrRunning` also when **another pod** holds it, and returns wrapped `ErrStopped`. DWX-221: Reactor Drain also runs due redrives. DWX-223: inbox Drain covers every row due at call time and gives `ErrRunning` while Run is active, but inbox has no leader, so "another pod" can't apply. DWX-222: scheduler uses `Tick(ctx, now)`, not Drain. DWX-215 says "possibly the rebuild"; DWX-216 says no. | **DWX-220** for the consumer, **DWX-216** for the rebuild. Document per package; there's no `Group.Drain` (gap G9). |
| C12 | **Projection API shape.** DWX-216/ADR-0005 have `projection.New(name, Version{N,h}, Shadow{…})` and a package function `CaughtUp(ctx,name)`. DWX-220 has `projection.New(rt, Config{Name, EventTypes, Active Version{N,Handler,Clear}, Shadow *Version})` and the methods `p.CaughtUp`/`p.Reset`. | **DWX-220.** |
| C13 | **Stream lock key.** DWX-209: FNV of the stream UUID only. DWX-211: `fnv64(tenant‖0x00‖stream_id)`. | **DWX-211** (DWX-209 carries the revision comment). |
| C14 | **Reactor failure.** DWX-220's input to DWX-221 says retries then Stopped. DWX-221: park, and Stopped only on park-write failure. | **DWX-221 / ADR-0007.** |
| C15 | **`changes` notify payload.** DWX-211 fixes JSON `{"tenant","entity","id"}`. DWX-220 has `projection.Notify(ctx, payload)` with a free payload. | Undecided. Either Notify takes `{Entity, ID}` and stamps the tenant from ctx, or the payload is free. |
| C16 | **Command span name.** DWX-214 has `{command} {stream type}`, but `Repository.Create/Update` (DWX-212) takes no command name. | Undecided. Add a name argument/option, or use `create`/`update`. |
| C17 | **Tenant.** These are consistent: every write path takes the tenant from ctx; consumers are global and the runner sets the tenant per Handle (projections included); the scheduler has no tenant; the inbox tenant is optional. Watch-out: today `ctxutil.TenantID` returns `("", true)` when claims exist without a tenant, so `""` must count as absent for `ErrNoTenant`. | n/a |

## 2. Gaps

**Blocking hand-off**
- **G1 `events` column list and `Envelope` field list.** Only fragments exist: TenantID (DWX-211), EventID (DWX-221), `trace_context` (DWX-214), Actor/Correlation/Causation (CONTEXT.md, the reactor prototype). Needs: names, types, nullability, `occurred_at` vs commit time, `schema_version`, and whether Envelope carries raw `Data` or the decoded event.
- **G2 ctxutil metadata API.** DWX-221/222/223 stamp actor `system:<…>`, correlation and causation; slogutil is to log `correlation_id`. No ticket names `WithActor/WithCorrelationID/WithCausationID`, how a user request's actor is derived (`Claims.UserID`?), or whether correlation defaults to the request ID.
- **G3 Full `schema.sql` inventory with owner package.** `events` (immutable + truncate + notify triggers, the 2 indexes from DWX-227), `consumer_cursors` (+`event_types`, stopped/_at/_error), `consumer_pending()`, `parked_events` (+hold row per stream, DWX-221), `guards`, `identifier_sequences`, `scheduler_runs`, `scheduler_intervals`, `inbox_messages`. Nobody has written the consolidated list. CI's `pgtest` depends on it.
- **G4 `lease.Claims` API.** The consumer Runtime, scheduler and inbox telemetry all require it, but there's no API: session vs xact lock, connection-per-claim, class allocation, and the `schema‖name` hash.
- **G5 Release/versioning plan.** Listed as open on the map. It covers breaking changes, wiedersehen, and where UPGRADING snippets live.
- **G6 Command naming for spans (C16), and the `changes` payload (C15).**

**Can be decided during implementation**
- G7 `notify.Listener` API (fixes the PgWaker leak), channel quoting (`"public.events"` needs a quoted LISTEN identifier), and a subscriber API for `changes` across pods.
- G8 `PgStore`/`InMemoryStore` constructor config (Pool, Codec, Schema, WithClock), and where `WithDefaultTenant` and `ErrNoTenant` live (ctxutil? eventstore? each of guard/numbering?).
- G9 `consumer.Group` Drain/Status; in-memory Await/Notify semantics in `InMemoryRuntime`; Cursor transaction in-memory via `postgres.Stage`.
- G10 `guard`/`numbering` public API (port the CPQ shape plus tenant).
- G11 Defaults: consumer `Retry{MaxAttempts,Base,Max}`, batch size, poll interval, Await poller interval, and the `consumer_pending` cap. Scheduler `RetryAfter` is referenced but missing from `Job`/`Config`.
- G12 Sentinel catalogue: `ErrNoTenant`, `ErrNotFound`, `ErrAlreadyExists`, `ErrConcurrencyConflict`, `ErrNestedUnitOfWork`, `ErrStreamMismatch`, `ErrNotCommandTransaction`, `consumer.ErrDuplicate/ErrRunning/ErrStopped`, `reactor.ErrStartAtRequired/ErrNotParked`, inbox `ErrRunning`, scheduler `ErrDuplicate`. These need one place per package and decisions on cross-package reuse.
- G13 Tenant-ID validation (non-empty, length/charset). It's opaque TEXT today.
- G14 `eventstore.Cursor` wire encoding for clients that send `min` to `Await` (string token?).
- G15 The UoW RLS option `SET LOCAL app.tenant_id` (DWX-211) is missing from DWX-209's option list.
- G16 There is no `.golangci.yml` in the repo (CI uses the action's defaults). The depguard rules for `aggregate` and `aggregatetest` need a new config, and that config may surface new lint findings.
- G17 Stream IDs are `~[16]byte`, so "IDs from external systems" (DWX-211) must be UUIDs. Document this.

## 3. Breaking changes in existing packages (verified against `main`)

| Package | Today | Decided change | Ticket |
|---|---|---|---|
| `pkg/postgres` | `Transaction{Tx() pgx.Tx}`; `UnitOfWork.Execute(ctx, fn(ctx, tx Transaction))`; `PgUnitOfWork`, `InMemoryUnitOfWork` (no-op, `nilTransaction`); `WithTx`, `NewPool` | **Break:** `Transaction` removed, `Execute(ctx, fn(ctx) error)`, silent join of an open tx. New: `TxFromContext`, `Conn`/`Querier`, `TxOptions` option, `Stage` with real in-memory rollback, optional RLS. `WithTx` stays. wiedersehen has 13 `tx.Tx()` sites. | DWX-209, 211 |
| `pkg/ctxutil` | `WithRequestID`, `WithClaims`; `TenantID` reads claims only | Add `WithTenantID`; `TenantID` checks explicit first, then claims (semantic change). Actor, correlation and causation needed (G2). | DWX-211 (+G2) |
| `pkg/otel` | `Config{ServiceName, ServiceVersion}`; explicit histograms; no `service.instance.id`; propagator TraceContext+Baggage | **Behavioural break:** Base2 exponential histograms by default (`Config.Histograms` fallback); sets `service.instance.id`, which changes series identity. | DWX-214, 217 |
| `pkg/slogutil` | `Setup` installs a plain text/json handler | `ContextHandler` installed by default (`request_id`, `tenant_id`, `user_id`, `trace_id`, `span_id`, later `correlation_id`). | DWX-214 |
| `pkg/connectrpc/logging`, `recovery` | Add `request_id` (+`user_id`) manually | **Break:** manual fields dropped; services without `slogutil.Setup` lose them. | DWX-214 |
| `pkg/nats` | `nats.reconnects.total` | Rename to `nats.reconnects` (outside the map). | DWX-218 |
| Repo/CI | No golangci config; plain `go test -race` | New `.golangci.yml` (depguard); testcontainers test dependency; PG 15/18 matrix; integration tests fail without Docker unless `-short`. | DWX-208, 215, 224 |

## 4. Packages to build

| Package | Public surface (one line) | Depends on (new) | Tickets |
|---|---|---|---|
| `internal/backoff` | Jittered exponential schedule | none | 208 |
| `internal/telemetry` | Attribute keys, metric/span names, 1 ms–10 s boundaries, scope helper | none | 214, 217 |
| `internal/pgtest` | `New(t) *pgxpool.Pool` (template DB from schema.sql, `GOUTILS_TEST_PG_IMAGE`) | schema.sql | 215 |
| `docs/postgres/schema.sql` | All tables/triggers/`consumer_pending`, PG≥15 `DO` guard, `public` | none | 213, 219, 221–223, 225, 227 |
| `pkg/postgres` (rewrite) | See §3 | ctxutil | 209, 211 |
| `pkg/postgres/notify` | `Listener` (LISTEN fan-out, wake-ups) | postgres | 208, 206 |
| `pkg/postgres/lease` | `Claims` (advisory-lock claims, class, `schema‖name` keys); Pg-only | postgres | 208, 213 |
| `pkg/postgres/guard`, `numbering` | Tenant-scoped uniqueness guards / gapless sequences inside the ctx tx; Pg + InMemory | postgres, ctxutil | 209, 211, 215 |
| `pkg/eventsourcing/aggregate` | `Event{EventType}`, `ID ~[16]byte`, `Root[I]` (sealed), `Base[I]`, `Raise`; internal accessor | none (stdlib+uuid) | 210, 212, 224 |
| `…/aggregate/aggregatetest` | `New[I,A](id, history...)`, `Raised(r)` | aggregate | 224 |
| `pkg/eventsourcing/eventstore` | `NewCodec`, `Register[E]`, `Upcaster`, `UpcastObject`, `Codec.Upcast`, `CheckCodec`; `Envelope`, `Cursor`; `PgStore`/`InMemoryStore` (+`WithClock`); `UnitOfWork` (`Execute`/`Within`); `NewRepository[I,A]` (`Create`/`Update`/`ExpectVersion`/`Load`/`LoadAt`/`History`/`Save`); `CommandResult`; sentinels; spans/metrics | aggregate, postgres, ctxutil, telemetry | 209–212, 214, 215, 224, 225 |
| `pkg/eventsourcing/consumer` | `NewPgRuntime`/`NewInMemoryRuntime`, `rt.Resume`; `New(rt, Config{Name,EventTypes,Mode,Handler})`, `Run`/`Drain`/`Status`/`Await`; `Group`; `NewRouter`/`On[E]`; `Permanent`; `Retry`, options | eventstore, postgres, notify, lease, backoff, telemetry, ctxutil | 214, 215, 219, 220, 227 |
| `pkg/eventsourcing/projection` | `New(rt, Config{…Active, Shadow})`, `Consumers`, `Await`, `CaughtUp`, `Reset`, `Notify` | consumer, notify | 216, 220 |
| `pkg/eventsourcing/reactor` | `New(rt, Config{Name,Handler,EventTypes,StartAt})`, `Consumer`, `Drain`, `Status`, `Parked`, `Redrive`, `Discard`; `WithRetry/WithTimeout/WithRedrive/WithLogger` | consumer, eventstore, backoff, ctxutil | 221, 225 |
| `pkg/scheduler` | `Job`, `Run`, `Outcome`, `Date`, `Config{Location…}`, `Register`, `Run(ctx)`, `Tick(ctx, now)` | postgres, lease, ctxutil, telemetry | 222 |
| `pkg/inbox` | `Store` (`Insert/List/Redrive/Discard/Cleanup`), `CleanupJob`, `Worker` (`Run`/`Drain`), `Ignore`, `Permanent` | postgres (UoW), lease, notify, scheduler, ctxutil, telemetry | 223 |

## 5. Suggested implementation issues

**Phase 0: spec closure (no code)**
1. `docs(eventsourcing): close hand-off gaps G1–G6`: Envelope/events columns, ctxutil metadata, schema table list, lease API, command naming, `changes` payload; reconcile C1/C2/C4/C7/C12 in ADRs and CONTEXT.md. Depends on: none.
2. `docs: release plan for breaking v0.x changes`: versioning, UPGRADING snippets, wiedersehen migration (G5). Depends on: 1.

**Phase 1: foundations**
3. `chore(ci): add golangci config, testcontainers and PG matrix`: `.golangci.yml`, `internal/pgtest`. Depends on: 4.
4. `docs(postgres): add schema.sql for event sourcing`: the full schema plus `consumer_pending`, immutability triggers. Depends on: 1.
5. **BREAKING** `feat(ctxutil): add tenant, actor and correlation context`. Depends on: 1.
6. **BREAKING** `feat(postgres)!: tx-in-context unit of work`: `Conn`, `Querier`, `Stage`, `TxOptions`, RLS option. Depends on: 5.
7. **BREAKING** `feat(otel)!: native histograms and service.instance.id`, plus `internal/telemetry`. Depends on: none.
8. **BREAKING** `feat(slogutil)!: context handler with fixed fields`: drop the manual fields in `connectrpc/logging` and `recovery`. Depends on: 5.
9. `feat(postgres): add notify listener and lease claims`, plus `internal/backoff`. Depends on: 3, 6.
10. `feat(postgres): add tenant-scoped guard and numbering`: Pg + InMemory with a contract suite. Depends on: 3, 5, 6.

**Phase 2: write side**
11. `feat(eventsourcing): add aggregate and aggregatetest`: depguard rule. Depends on: 3.
12. `feat(eventstore): add JSON codec and upcaster chain`, plus `CheckCodec`. Depends on: 11.
13. `feat(eventstore): add stores, command transaction and repository`: Pg/InMemory, spans/metrics, contract suite. Depends on: 4, 6, 7, 12.

**Phase 3: read side**
14. `feat(consumer): add runtime, consumer and group`: event-type reads (DWX-227), claim check, Stopped/Resume, Status/Await/Drain, gauges. Depends on: 9, 13.
15. `feat(projection): add versioned projections`: Active/Shadow, Notify, Reset, guard-snippet docs. Depends on: 14.
16. `feat(reactor): add reactor with parking and redrive`. Depends on: 14.

**Phase 4: standalone runners**
17. `feat(scheduler): add daily and interval jobs`. Depends on: 4, 5, 9.
18. `feat(inbox): add inbox store and worker`. Depends on: 6, 9, 17.

**Phase 5**
19. `docs(eventsourcing): add quickstart and projection bump guide` (DWX-226). Depends on: 15, 16.
20. `chore(nats)!: rename reconnects metric` (DWX-218). Independent. **BREAKING.**

Breaking: 5 (semantic), 6, 7, 8, 20.
