# Tenant column with per-tenant stream identity and global consumers

All tenants share one set of tables in one Postgres schema, and every library table written by commands carries a `tenant_id TEXT NOT NULL`. Stream identity is `(tenant, stream_id)` rather than a globally unique UUID, because stream IDs may be assigned by external systems: a colliding or deliberately chosen ID must stay confined to one tenant, including its stream lock. Consumers, projections and the scheduler stay global, though. The isolation they would gain per tenant is small, and the runtime cost is large.

## Considered Options

- **A Postgres schema per tenant, with no tenant concept in the library.** This was rejected as the main model because every tenant would multiply consumers, claimed connections and wiring, and onboarding would touch the runtime. It stays possible as a deployment choice through the `Schema` option.
- **A database per tenant.** This was rejected because pool-per-tenant management is beyond a library.
- **Per-tenant consumers with a Cursor per `(consumer, tenant)`.** This was rejected because Reactors already isolate failures per event by parking them, and a failing projection is a code or upcaster bug that usually affects every tenant. The cost would have been a tenant registry, a new claim model and tenant-targeted wake-ups.
- **A tenant-aware scheduler with per-tenant timezones.** This was rejected because tenant lists and timezones are app knowledge, so a job iterates over tenants itself.

## Consequences

- Every write path needs a tenant in the context (`ctxutil.WithTenantID` or the claims), otherwise it fails with `ErrNoTenant`. An empty tenant counts as absent. Single-tenant services set a fixed tenant with `ctxutil.WithTenantID` in middleware or the root context; there is no library option for it.
- The stream lock key hashes the schema, the tenant and the stream ID together (`fnv64(schema‖0x00‖tenant‖0x00‖stream_id)`).
- A poison event in a projection stops it for all tenants.
- `pg_snapshot_xmin` is database-wide, so a long transaction in one tenant delays consumer visibility for all tenants. This costs latency, not correctness.
- Guards and numbering are scoped by tenant, so each tenant has its own gapless ranges.
- App-owned read models must filter by tenant themselves. RLS through the UoW's `SET LOCAL app.tenant_id` is available, off by default.
