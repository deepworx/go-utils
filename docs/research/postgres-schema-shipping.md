# How Go libraries ship a Postgres schema to their consumers

Research for [DWX-207](https://linear.app/deepworx/issue/DWX-207/how-go-libraries-ship-a-postgres-schema-to-their-consumers)
(child of map DWX-205, "Postgres-only event sourcing library in go-utils").
Researched 2026-10-17 against the current default branches of the projects cited.

## Question

What are the established ways for a Go library to deliver the PostgreSQL schema it needs
(tables, triggers, functions, views) to the services using it, and what are their trade-offs?
For each: library upgrades that change the schema, configurable table names / Postgres schema,
multiple instances (tenants) in one database, coexistence with the app's own migrations.

Our schema: `events` (`transaction_id xid8`, immutability + `pg_notify` triggers),
`consumer_cursors`, `parked_events`, `scheduler_runs`, `webhook_inbox`, `guards`,
`identifier_sequences`, versioned projection tables behind views.

## TL;DR

- Four patterns exist in the wild: (A) idempotent auto-init at runtime, (B) a library-owned
  migrator with its own version table, (C) embedded SQL handed to the app's migrator,
  (D) SQL the app copies. Mature libraries combine **B as the primary path with C/D as an export
  escape hatch** (River, Marten, Emmett/Pongo).
- The Postgres **schema name** is the configuration knob that works. Configurable table names
  (watermill) don't scale to a set of tables, triggers and functions that reference each other.
- **Recommendation:** a River-style `Migrate(ctx, pool, cfg)` with embedded, numbered, immutable,
  schema-templated SQL, its own version table *inside the target schema*, a per-schema advisory
  lock, and one transaction per migration. Also export the rendered SQL per version for teams on
  goose/atlas, plus a `Validate`/"ensure migrated" check so runtime code never needs DDL rights.
  Schema-per-tenant then means calling `Migrate` once per schema.

## The four patterns

### A. Idempotent auto-init at runtime (`CREATE ... IF NOT EXISTS` on first use)

**watermill-sql.** The publisher and subscriber run `SchemaInitializingQueries` when
`AutoInitializeSchema` / `InitializeSchema` is set
([publisher.go](https://github.com/ThreeDotsLabs/watermill-sql/blob/master/pkg/sql/publisher.go),
[subscriber.go](https://github.com/ThreeDotsLabs/watermill-sql/blob/master/pkg/sql/subscriber.go)).
The PostgreSQL adapter emits `CREATE TABLE IF NOT EXISTS "watermill_<topic>" (... "transaction_id" xid8 ...)`.
It wraps that in a transaction behind `pg_advisory_xact_lock`, because concurrent
`CREATE TABLE IF NOT EXISTS` can still fail with 23505
([schema_adapter_postgresql.go](https://github.com/ThreeDotsLabs/watermill-sql/blob/master/pkg/sql/schema_adapter_postgresql.go)).
Table names come from `GenerateMessagesTableName func(topic string) string` and
`GenerateMessagesOffsetsTableName` (same file,
[offsets_adapter_postgresql.go](https://github.com/ThreeDotsLabs/watermill-sql/blob/master/pkg/sql/offsets_adapter_postgresql.go)).
The library has no version table and no upgrade path: the DDL is "make sure it exists", nothing more.
Publishing with `AutoInitializeSchema` inside a transaction is refused outright (publisher.go).

**Pongo / Emmett default.** The default `autoMigration: 'CreateOrUpdate'` creates tables on first
operation ([Pongo schema-migrations.md](https://github.com/event-driven-io/Pongo/blob/main/src/docs/schema-migrations.md)).
Unlike watermill, this runs through a versioned migrator (see B).

| Concern | Behaviour |
|---|---|
| Upgrades | None. `IF NOT EXISTS` never alters an existing table, so schema changes need manual SQL. |
| Configurable names | Per-table name callbacks. Works for one or two flat tables, but not for triggers or functions that reference other objects by name. |
| Multiple instances | Per-topic tables, or schema-qualified names returned by the callback. |
| Coexistence | Invisible to the app's migrator. The runtime DB user needs `CREATE`. |

Verdict: fine for a single append-only table. It doesn't fit a schema with triggers, functions
and views that will evolve.

### B. Library-owned migrator with its own version table

**River (`rivermigrate`)** is the reference design for Go.
- SQL is embedded per driver: `//go:embed migration/*/*.sql`, with numbered `NNN_name.up.sql` / `.down.sql` files
  ([river_pgx_v5_driver.go](https://github.com/riverqueue/river/blob/master/riverdriver/riverpgxv5/river_pgx_v5_driver.go),
  [migration/main/](https://github.com/riverqueue/river/tree/master/riverdriver/riverpgxv5/migration/main)).
- `rivermigrate.New(driver, &Config{Line, Schema, Logger})`. It exposes `Migrate`, `MigrateTx`,
  `Validate`, `ValidateTx`, `ExistingVersions`, and `GetVersion`, with `MigrateOpts{MaxSteps, TargetVersion}`
  ([river_migrate.go](https://github.com/riverqueue/river/blob/master/rivermigrate/river_migrate.go)).
- **Schema** is injected by templating, not `search_path`. Every object reference in the SQL is written
  `/* TEMPLATE: schema */river_job`, and the migrator substitutes the prefix
  (e.g. [006_bulk_unique.up.sql](https://github.com/riverqueue/river/blob/master/riverdriver/riverpgxv5/migration/main/006_bulk_unique.up.sql)).
  The docs say River "explicitly prefix[es] all table, function, and enum references", and that
  "use of `search_path` is generally not recommended" ([alternate schema](https://riverqueue.com/docs/alternate-schema)).
- **Version table** `river_migration`, which is itself migration 001
  ([001_create_river_migration.up.sql](https://github.com/riverqueue/river/blob/master/riverdriver/riverpgxv5/migration/main/001_create_river_migration.up.sql)).
  It was later rebuilt to `(line, version)` so add-ons (River Pro "lines" such as `workflow` or `sequence`)
  can version independently in the same table
  ([005_migration_unique_client.up.sql](https://github.com/riverqueue/river/blob/master/riverdriver/riverpgxv5/migration/main/005_migration_unique_client.up.sql)).
- **Upgrades:** each release that adds a migration carries a "⚠️ contains a new database migration,
  version N" banner in the CHANGELOG with `river migrate-up` and `migrate-get` instructions
  ([CHANGELOG.md](https://github.com/riverqueue/river/blob/master/CHANGELOG.md)). Migrations run one
  transaction each, and the docs say "Do not wrap the complete migration history in one application transaction".
  The migrator does not lock by itself; the docs say to wrap it in an advisory lock and to
  "use a single migration process for a deployment" ([migrations](https://riverqueue.com/docs/migrations)).
- **Export path for other migrators:** `river migrate-get --version N --up [--schema s]` prints the SQL
  so teams can paste it into goose and similar tools. The docs ask users to "preserve version boundaries"
  ([migrations](https://riverqueue.com/docs/migrations), CHANGELOG PR #903 for `--schema`).
- **Cost of supporting both paths:** migration 005 contains
  "Tolerate users who may be using their own migration system rather than River's ... this table won't exist"
  (`IF to_regclass(...river_migration) IS NOT NULL`). Once you export SQL, every later migration has to
  work whether or not your version table exists.

**Emmett (PostgreSQL event store) on Dumbo.**
- The event store calls a migrator lazily unless `schema.autoMigration === 'None'`
  ([postgreSQLEventStore.ts](https://github.com/event-driven-io/emmett/blob/main/src/packages/emmett-postgresql/src/eventStore/postgreSQLEventStore.ts)).
- The migration chain is a set of named past migrations (`0_38_7`, `0_42_0`, `0_43_0`, each with snapshot tests)
  plus one idempotent "current schema" migration (`emt:postgresql:eventstore:initial`, `ignoreHashMismatch: true`)
  ([migrations/index.ts](https://github.com/event-driven-io/emmett/blob/main/src/packages/emmett-postgresql/src/eventStore/schema/migrations/index.ts)).
  The current-schema SQL is written with `IF NOT EXISTS` / `OR REPLACE` throughout
  ([tables.ts](https://github.com/event-driven-io/emmett/blob/main/src/packages/emmett-postgresql/src/eventStore/schema/tables.ts)).
- Configuration covers `databaseSchemaName`, `projectionsDatabaseSchemaName` and `migrationTable` (schema/table).
  The migration table defaults to the event-store schema
  ([eventStoreDatabaseSchema.ts](https://github.com/event-driven-io/emmett/blob/main/src/packages/emmett-postgresql/src/eventStore/schema/eventStoreDatabaseSchema.ts)).
  When a custom schema is set, only the current-schema migration runs, with no legacy chain (same index.ts).
- Tenancy uses a `partition TEXT` column with `PARTITION BY LIST (partition)` on streams, messages and
  processor tables (tables.ts). Emmett picked a row-level partition key over schema-per-tenant.
- Dumbo records each applied migration plus a hash of its SQL in `dmb_migrations` (configurable).
  It aborts on a hash mismatch unless told to ignore or override it, and on PostgreSQL it takes an advisory lock
  ([Pongo schema-migrations.md](https://github.com/event-driven-io/Pongo/blob/main/src/docs/schema-migrations.md),
  [dumbo migrations.ts](https://github.com/event-driven-io/Pongo/blob/main/src/packages/dumbo/src/storage/postgresql/core/schema/migrations.ts),
  [migrationTableComponent.ts](https://github.com/event-driven-io/Pongo/blob/main/src/packages/dumbo/src/core/schema/migrators/migrationTableComponent.ts)).
- Three runtime modes: `CreateOrUpdate`, `EnsureMigrated` (read-only history check that throws
  `PendingMigrationsError`; "the application's database user needs read access to the history table"), and `None`.
  The docs recommend migrating in a deployment step and running instances with `EnsureMigrated`.
  Schema components compose, so "a library such as Emmett can add them to its own schema and migrate everything in one call"
  (schema-migrations.md).

**Marten (.NET, via Weasel)** uses diff-based migration instead of numbered files.
- Modes are `AutoCreate` `All` / `CreateOrUpdate` (the default) / `CreateOnly` / `None`.
- `ApplyAllConfiguredChangesToDatabaseAsync` / `db-apply` applies changes, `AssertDatabaseMatchesConfigurationAsync` / `db-assert` checks for drift, and `db-patch` / `WriteMigrationFileAsync` exports SQL plus a drop script for a DBA pipeline.
- The team's recommended approach is the CLI
  ([Marten migrations](https://martendb.io/schema/migrations.html)).
- `StoreOptions.DatabaseSchemaName` moves every object, including `mt_events`
  ([StoreOptions.cs](https://github.com/JasperFx/marten/blob/master/src/Marten/StoreOptions.cs),
  [EventGraph.cs](https://github.com/JasperFx/marten/blob/master/src/Marten/Events/EventGraph.cs)).
- Tenancy is either conjoined (`tenant_id` column) or database-per-tenant, and migrations are applied
  "to all the configured tenant databases on startup". Schema-per-tenant inside one database is explicitly
  rejected: "That's a hard no" ([multitenancy](https://martendb.io/configuration/multitenancy.html)).
- Diffing needs a full schema model (Weasel). That costs too much for a Go library and is noted only for comparison.

| Concern | Behaviour |
|---|---|
| Upgrades | New numbered migration per release. The library controls order and data backfills. |
| Configurable names | Schema name (River, Emmett, Marten). None of the three lets you rename individual tables. |
| Multiple instances | One version table per schema, so each schema migrates independently. |
| Coexistence | Separate version table, so no version-number collision with the app's goose/migrate history. The app runs the library migrator before (or alongside) its own. |

### C. Embedded SQL exposed as `fs.FS` for the app's migrator

The library embeds its migrations and exports an `fs.FS`. The app points its own tool at it.

- **goose:** `goose.NewProvider(dialect, *sql.DB, fsys fs.FS, opts...)` accepts an `embed.FS` / `fs.Sub`
  ([provider.go](https://github.com/pressly/goose/blob/main/provider.go)).
  `WithTableName` (default `goose_db_version`) makes a *second* provider with its own history table
  possible, e.g. `es_goose_db_version`. Other options are `WithSessionLocker` (Postgres advisory lock, default lock ID
  `4097083626`, overridable via `WithLockID`) and `WithAllowOutofOrder`
  ([provider_options.go](https://github.com/pressly/goose/blob/main/provider_options.go),
  [lock/session_locker_options.go](https://github.com/pressly/goose/blob/main/lock/session_locker_options.go)).
  The only parametrisation is `-- +goose ENVSUB ON` (environment variable substitution)
  ([sqlparser/parser.go](https://github.com/pressly/goose/blob/main/internal/sqlparser/parser.go)).
  goose needs `database/sql`, so a pgx app has to bridge with `stdlib.OpenDBFromPool`.
- **golang-migrate:** `source/iofs` takes an `fs.FS`. The Postgres driver's `Config{MigrationsTable, SchemaName, ...}`
  keeps a **single row** `(version bigint, dirty boolean)`, which it truncates and rewrites, and the advisory lock ID
  is derived from database, schema and table
  ([postgres.go](https://github.com/golang-migrate/migrate/blob/master/database/postgres/postgres.go)).
  Library and app migrations therefore *cannot* share a table. Each needs its own `MigrationsTable`.
- **tern:** `NewMigratorEx(ctx, *pgx.Conn, versionTable, opts)` with `LoadMigrations(fsys fs.FS)`. Migrations are
  Go `text/template`s rendered with `Migrator.Data`, so `{{.schema}}` substitution is native. It uses a single fixed
  advisory lock number `9628173550095224`
  ([migrate.go](https://github.com/jackc/tern/blob/master/migrate/migrate.go)).
- **Atlas:** tracks revisions in `atlas_schema_revisions`. `--revisions-schema` and `--lock-name` let several
  migration directories share one database
  ([apply](https://atlasgo.io/versioned/apply)). Each directory is guarded by `atlas.sum`, so files copied in need
  `atlas migrate hash` ([directory integrity](https://atlasgo.io/concepts/migration-directory-integrity)).
  Composing external schemas (`composite_schema`, `external_schema`) requires a paid tier
  ([external schemas](https://atlasgo.io/atlas-schema/external)).

| Concern | Behaviour |
|---|---|
| Upgrades | The library adds files. The app picks them up on dependency bump and its tool applies them. Works only if the app gives the library its own history table (golang-migrate requires it; for goose it's strongly advisable to avoid number collisions). |
| Configurable names | Depends on the tool: tern templates natively, goose has only ENVSUB, golang-migrate none. The library would have to pre-render (`MigrationFS(schema)` returning an in-memory FS). |
| Multiple instances | One provider run per schema, each with its own history table. |
| Coexistence | Native for the app's tool. The cost is that the library's embedded SQL gets locked to a tool's file and annotation dialect (`-- +goose Up` vs `.up.sql`). |

Verdict: tying the library to one migrator's file format is a poor bet for a shared library.
A *rendered SQL export* covers the same needs without that commitment.

### D. Documented SQL the app copies

- **gue** (Go Postgres queue): "you need to apply [DB migration](migrations/schema.sql)". It ships one schema file
  and has no migrator ([README](https://github.com/vgarvardt/gue/blob/master/README.md)).
- **pgmq:** either `CREATE EXTENSION pgmq` or a SQL-only install, `psql -f pgmq-extension/sql/pgmq.sql`, into a
  fixed `pgmq` schema ([pgmq-extension README](https://github.com/pgmq/pgmq/blob/main/pgmq-extension/README.md)).
  Extension upgrades go through `ALTER EXTENSION pgmq UPDATE` with versioned update scripts. Gaps in that chain have
  stranded users, e.g. Supabase holding at 1.4.4
  ([UPDATING](https://pgxn.org/dist/pgmq/UPDATING.html),
  [Supabase changelog](https://supabase.com/changelog/39378-potential-breaking-change-in-pgmq-from-1-4-4-to-1-5-1-and-temporary-halt-on-upgr)).
  Shipping as an extension is out of scope for us because it needs server-side install rights.
- River's `migrate-get` and Marten's `db-patch` are the "copy SQL" path offered *on top of* a migrator.

| Concern | Behaviour |
|---|---|
| Upgrades | Manual. Consumers must diff releases and author their own migration, which is error-prone for triggers and functions. |
| Configurable names | Search-and-replace by hand. |
| Multiple instances | Copy once per schema. |
| Coexistence | Perfect, because it simply *is* an app migration. |

Verdict: acceptable only as an export of B, never as the sole mechanism.

## Cross-cutting findings for our schema

1. **Schema name, not table names.** Our objects reference each other: triggers call functions,
   views select projection tables, and `pg_notify` payloads name channels. River, Emmett and Marten all parametrise
   only the schema, and render it as an explicit qualifier rather than relying on `search_path`.
   Per-table name callbacks (watermill) multiply configuration and break cross-references.
2. **`pg_notify` channels and advisory-lock keys must include the schema**, or two instances in one database
   cross-talk. Golang-migrate derives its lock ID from database+schema+table (postgres.go). goose and tern use one
   fixed ID, so concurrent migrations of different schemas serialise needlessly.
3. **Version table inside the target schema** (Emmett's default, River with `Schema` set) makes schema-per-tenant
   self-contained: `DROP SCHEMA ... CASCADE` removes the tenant including its history, and provisioning a tenant is
   `CREATE SCHEMA` + `Migrate`.
4. **One transaction per migration, advisory lock around the run.** This matches River's guidance and watermill's
   23505 workaround. Our `xid8` / `pg_current_xact_id()` design has no problem with transactional DDL.
5. **Runtime should not need DDL rights.** Pongo's `EnsureMigrated` and Marten's `db-assert` both separate
   "migrate in a deployment step" from "verify at startup". River's `Validate` does the same.
6. **Supporting an external-migrator path has a recurring cost.** River migration 005 must tolerate a missing
   version table. Emmett keeps the legacy chain only for the default schema. Decide up front whether the export path
   writes the library's version row (simplest: the exported SQL includes the `INSERT` into our version table).
7. **Immutability and release discipline.** Dumbo hashes applied SQL and refuses mismatches. River flags each new
   migration in the CHANGELOG. Both treat shipped migrations as immutable. With release-please in this repo, a
   `feat(es)!:` or explicit CHANGELOG note per migration is the equivalent.

## Recommendation for go-utils

Use pattern **B, River-shaped**, with exports for C/D:

- Put `//go:embed migrations/*.sql` in the event-sourcing package, as numbered, forward-only `NNN_name.sql`
  files. Skip down migrations: they are destructive for an event store, and River warns its full down
  "will remove all River schema additions and delete any data". Every object reference uses a schema
  placeholder and is rendered as a quoted identifier.
- Expose `Migrate(ctx, pool *pgxpool.Pool, cfg MigrateConfig) error` with `MigrateConfig{Schema string; TargetVersion int; Logger *slog.Logger}`
  (CS-5, CTX-1). It should:
  - create the schema if it is missing;
  - keep a version table `<schema>.es_migrations(version, applied_at, checksum)`;
  - take `pg_advisory_lock(hashtext('<schema>'))` (or similar), run each migration in its own transaction,
    and record a checksum to detect edited migrations (Dumbo-style).
- Expose `Validate(ctx, pool, schema)`, which returns a sentinel `ErrPendingMigrations` listing missing versions,
  for startup and readiness checks, so service runtime users need no `CREATE`.
- Expose `SQL(schema string) ([]Migration, error)` (version, name, rendered SQL including the version-row
  insert) for teams that want goose/atlas/tern. They wrap each version as one migration in their own tool.
  This replaces shipping a tool-specific `fs.FS`.
- Make every schema-scoped runtime component (store, consumers, LISTEN channel) take the same `Schema` option,
  and derive the `pg_notify` channel from it.
- **Multi-tenancy:** schema-per-tenant equals one `Migrate` call per schema, and `Validate` per schema at startup.
  For large tenant counts, the version table per schema keeps upgrades restartable per tenant. If tenancy instead
  goes row-level (Emmett-style `partition` column), nothing in this mechanism changes.
- **Projection tables behind views:** the library migrations own only the library tables. App-defined projections
  belong in the app's own migrations or a separate, app-registered migration "line" (River's `line` concept), so the
  library's version sequence never depends on app code.
- **Coexistence:** the app runs `es.Migrate` first (e.g. in its migrate command or init container), then its own
  goose/atlas migrations. The two histories live in different tables and never collide.

## Open questions for the map

- Is checksum enforcement wanted (hard failure on edited migrations), or only a warning?
- Should the library ever auto-migrate at startup (an opt-in `AutoMigrate` for prototypes), or always require an
  explicit call?
- Do we need River-style migration "lines" from day one, for app projection migrations or optional modules such as
  `webhook_inbox`?
