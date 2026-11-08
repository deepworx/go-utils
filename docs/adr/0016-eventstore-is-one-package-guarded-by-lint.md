# Eventstore is one package, and lint guards the app layer

`pkg/eventsourcing/eventstore` holds the Repository, the `Store` and `UnitOfWork` interfaces, and both backends: `PgStore` with `PgUnitOfWork`, and `InMemoryStore` with `InMemoryUnitOfWork`. They share one package because `Store` is sealed. Its methods `load`, `append`, `findCommand`, `recordCommand` and `now` are unexported, so only this package can implement it. They carry the invariants of the Command transaction: events are appended only inside the Command transaction that holds their Stream, after the Expected version check, and a Command ID is recorded in that same transaction (ADR-0012). A backend in another package would need these methods exported, and then any caller could append outside a Command transaction.

An app package that uses the Repository can therefore also call `eventstore.NewPgStore` and `eventstore.NewPgUnitOfWork`. Lint rules, not a package boundary, keep it from doing so. In `.golangci.yml`, for the non-test files of `internal/quickstart/app`:

- the depguard rule `quickstart-app-no-pgx` denies `github.com/jackc/pgx/v5` and its subpackages;
- forbidigo forbids calls to `eventstore.NewPg…`.

The quickstart server wires the Postgres backends and is not restricted.

## Considered Options

- **Split the backends into subpackages**, such as `eventstore/pg` and `eventstore/inmemory`. This was rejected because of three costs. `Store` would need a public interface, which exposes the invariants of the Command transaction. The split would not take otel or pgx out of an app's build graph: the Repository's instrumentation imports otel, directly and through `internal/telemetry`, and the in-memory backend imports `pkg/postgres` and through it pgx. And it would break the import path of every service.

## Consequences

- An app package's build graph includes pgx and otel, through `eventstore` as well as `consumer`, `projection` and `pkg/postgres`. Only the lint rules keep pgx and the eventstore's Postgres constructors out of its code.
- Tests on the in-memory backends stay database-free at runtime, although pgx is compiled in.
- The quickstart's `readmodel.go` is an exception to the depguard rule: it holds `PgReadModel`, the Postgres adapter of the `task_list` read model, which needs pgx. A service puts its Postgres adapters outside its app packages, so its rule needs no such exemption. forbidigo still covers `readmodel.go`.
- A service applies this by copying the quickstart's depguard rule, the forbidigo settings and the two forbidigo exclusion rules, and pointing their file globs and paths at its own app packages.
