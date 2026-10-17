# The event log is append-only and tenant data is not erased

The library never deletes or rewrites events, and offboarding a tenant removes nothing: the service stops serving the tenant, and its events, guards, numbering, Parked rows and projections stay. `docs/postgres/schema.sql` enforces this with a row trigger that rejects `UPDATE` and `DELETE` on `events` and a statement trigger that rejects `TRUNCATE`. What a service puts into event payloads, personal data included, is the service's responsibility; the library makes no statement about it.

## Considered Options

- **Hard delete per tenant** (`DELETE … WHERE tenant_id = $1` across all tables). This was rejected because an event log that can be rewritten is no longer the source of truth: Reactors, projections and audits rely on history never changing.
- **Crypto-shredding with a per-tenant key in the Codec.** This was deferred because it brings key management into a library meant for quick prototypes, and it still needs a hard delete for metadata and read models. The CPQ reference made the same call: its predecessor's per-tenant encryption was never wired up (`EraseKey` had no caller), and CPQ documents restriction instead of erasure. Because `event_data` is BYTEA, an encrypting Codec decorator can be added later without breaking the API.
- **A documented cleanup of the mutable tables only** (guards, numbering, Parked rows, Inbox). This was rejected because it gains nothing while the events remain, and a projection rebuild restores the read models from the log anyway.

## Consequences

- Blocking an offboarded tenant (authentication, authorization) is the app's job; the library has no tenant registry (ADR-0002).
- A Reactor command for a blocked tenant fails, and its Stream is Parked until auto-redrive is exhausted.
- An erasure request for data in the log cannot be met by the library; a service that expects such requests has to keep that data out of its events.
