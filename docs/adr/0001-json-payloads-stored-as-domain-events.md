# JSON payloads, stored as the domain events themselves

Event payloads are JSON (stdlib `encoding/json`), and the stored shape is the domain event struct itself: exported fields with mandatory `json` tags, no separate wire type and no mapper. The library serves new services and prototypes, which need to store their first event without `.proto` files, codegen or defining every event twice. This deliberately rules out Protobuf payloads mapped by hand-written converters.

## Considered Options

- **Protobuf only**, as in the reference. This was rejected because every prototype pays for codegen and mappers up front. `buf breaking` would catch incompatible schema changes, but it doesn't justify that cost here.
- **A format-agnostic core with a JSON default and an opt-in Protobuf subpackage.** This was rejected because the seam and a second format add surface area that no planned consumer needs.

## Consequences

- The `json` tags of a domain event are a storage contract. Renaming a tag without an upcaster changes how history is read.
- Decoding bypasses domain constructors. A stored event that today's invariants would reject still loads, because history is a fact.
- Decoding is lenient. Adding or removing a field needs no upcaster. A field whose zero value doesn't mean the same as "missing" in old events does need one.
- The schema version is not declared anywhere. It is the number of registered upcasters plus one.
- `event_data` stays `BYTEA`, not `JSONB`, so the stored bytes stay exactly as written.
