# Inbox messages are deduplicated per Source and Tenant

`Store.Insert` drops a message whose Source, Tenant and Key match a stored one, whatever its status. A Key therefore only has to be unique within its Source and Tenant, the same scope as Stream identity (ADR-0002) and Command IDs (ADR-0012). One Source often serves many Tenants or provider accounts, and their key spaces are not ours to coordinate: with dedupe per Source, a message of one Tenant would silently drop another Tenant's message with the same key, and the intake would still answer 202.

`tenant_id` is `TEXT NOT NULL DEFAULT ''`, and an empty value is a message without a Tenant, as `Message.Tenant == ""` is in Go. The constraint `inbox_messages_source_tenant_key_key UNIQUE (source, tenant_id, key)` then treats two messages without a Tenant as duplicates.

## Considered Options

- **Dedupe per Source, with tenant-qualified keys by contract.** This was rejected because every app would have to remember to prefix its keys (`acct_123:evt_456`), and a forgotten prefix only shows up as lost messages.
- **A nullable `tenant_id` with `UNIQUE NULLS NOT DISTINCT`.** This was rejected because Postgres parses the whole schema file before it runs the PostgreSQL 15 guard at its top (ADR-0003), so PostgreSQL 14 would fail with a syntax error instead of the guard's message.
- **A unique index on `COALESCE(tenant_id, '')`.** This was rejected because it cannot be a named constraint and every `ON CONFLICT` would have to repeat the expression.

## Consequences

- Dedupe does not authenticate. The Tenant usually comes from the request, such as a `/webhooks/{tenant}` path, so a forged message with a real Key still drops the real delivery within that Tenant. Intake must authenticate a message, for example by checking the provider's signature, before it calls `Insert`, and answer 401 without inserting when that fails. The Key must come from the authenticated content.
- A Handler that verifies again and finds a bad signature returns `Ignore`, not `Permanent`: a forgery needs no Operator.
- Parked messages are still never cleaned up and wait for an Operator (ADR-0014). Authenticating before `Insert` keeps forgeries from reaching them.
- The Correlation stays `inbox:<source>:<key>`, so two Tenants' messages with the same Key share it. Their ctx, span and logs carry the Tenant to tell them apart.
