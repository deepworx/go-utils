# Library schema shipped as documentation only

The Postgres schema the library needs (tables, triggers, functions) is shipped as documentation, not as code: one copyable file, `docs/postgres/schema.sql`, written for schema `public`, plus a ready-to-copy upgrade snippet in `docs/postgres/upgrades/` for every change to it. The library requires that the schema exists and never runs DDL. Services already have migration tooling (typically golang-migrate), and a second migrator or a tool-specific embedded FS would be one more thing to own, wire and keep compatible.

## Considered Options

- **A library-owned, River-style `Migrate`** with its own version table, advisory lock and checksums, as recommended by the schema-shipping research. This was rejected because it means building and maintaining a migrator next to the one every service already runs.
- **Embedded golang-migrate files exposed as `fs.FS`**, run by the app as a second source with its own history table. This was rejected because it ties the library's API to one tool's file format and still turns the library into a migration source.

## Consequences

- `schema.sql` always shows the complete current state. Every schema change adds a snippet `docs/postgres/upgrades/NNNN_<description>.sql`, numbered sequentially rather than by release version, that services copy 1:1 into their own next migration. Snippets follow expand/contract and stay backward compatible with the previous minor version, so the migration can run before new pods roll out.
- A schema change is always a breaking change: its commit carries `!` and a `BREAKING CHANGE:` footer that names the snippet to apply before deploying.
- There is no runtime schema check. New code on an outdated schema fails on its first query.
- The runtime `Schema` option stays (default `public`): it qualifies every name, and the `Pg*` constructors derive the explicit names they hand to `notify` and `lease` from it: channels `<schema>.events`, `<schema>.changes` and `<schema>.inbox`, lease namespaces `<schema>.consumer`, `<schema>.scheduler` and `<schema>.inbox`. Using another schema means editing the schema prefix and channel names in the copied SQL.
- The minimum Postgres version (15) is enforced by a `DO` block at the top of the documented schema, not by library code.
- go-utils' own integration tests load `docs/postgres/schema.sql` directly, so the documentation cannot drift from the code.
