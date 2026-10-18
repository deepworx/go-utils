# Operator APIs are cross-tenant and unauthorized

Some calls exist for Operators: trusted callers who keep consumers and queues running across all Tenants. They are Inbox `Store.List`, `Store.Redrive` and `Store.Discard`, Reactor `Parked`, `Redrive` and `Discard`, and `projection.Subscribe`. Their tenant comes from their arguments, never from the ctx Tenant, and the library performs no authorization on them. Inbox calls take a bare row ID, `Reactor.Parked(ctx, "")` lists every Tenant, Reactor `Redrive` and `Discard` take a `Stream{Tenant, ID}` as given, and `Subscribe` delivers every Tenant's `Change` on one shared `<schema>.changes` channel.

The only identity these calls check is who acted. Both `Discard`s record `ctxutil.ResolveActor`, which rejects a user ID with the reserved `system:` prefix (ADR-0010), so the audit trail cannot be chosen by the caller.

## Considered Options

- **Scope each call to the ctx Tenant.** This was rejected because an Operator works across Tenants by definition, and an Inbox message may have no Tenant at all. Every call would need an unscoped twin, which doubles the surface for a consumer that does not exist yet.
- **Add optional tenant filters and guards** (`Filter.Tenant`, `tenant_id IS NOT DISTINCT FROM $n` on Inbox Redrive and Discard). This was deferred, not rejected: nobody exposes these calls to tenant admins today, and their shape should follow a real caller.
- **One change channel per Tenant.** This was rejected because each `LISTEN` holds a connection, and filtering `Change.Tenant` in Go is one comparison for the consumer.

## Consequences

- An app must not expose these calls to tenant admins or end users without its own guard. Exposed as they are, they leak data across Tenants and allow guessing IDs (IDOR):
  - `Store.List` returns every Tenant's messages, including `Payload`.
  - `Parked.Cause` carries raw error text, which may contain details of another Tenant.
  - Inbox `Redrive` and `Discard` distinguish an unknown ID (`ErrNotFound`) from a known ID in the wrong status, so any ID can be probed.
- A `Subscribe` consumer that forwards changes, for example to browsers, must filter by `Change.Tenant`.
- Revisit this decision when a consumer needs tenant-admin self-service for parked work. Tenant-scoped variants would then be added beside these calls, not in place of them.
