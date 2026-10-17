# Tracing and OpenTelemetry conventions in event-sourced systems

Research for [DWX-217](https://linear.app/deepworx/issue/DWX-217/tracing-and-otel-conventions-in-event-sourced-systems)
(child of map DWX-205, feeds [DWX-214](https://linear.app/deepworx/issue/DWX-214/observability-for-the-consumer-core-metrics-interface-or-opentelemetry)).
Researched 2026-10-17 against OpenTelemetry semantic conventions `main` (latest release v1.44.0,
2026-08-04) and the current default branches of the projects cited.

## Question

What do the OpenTelemetry semantic conventions and mature event stores prescribe for tracing and
metrics across an event log, so the details of DWX-214 follow established practice?

Already decided, not re-opened here: OTel directly via global providers; library spans for the
Command transaction, Consumer batch / per-event handling and Inbox processing; a nullable
`trace_context` column on `events` and on the Inbox storing the W3C `traceparent` at append time;
Consumer spans **link** to it instead of parenting.

## TL;DR

- **Messaging semconv is still `Development`** (spans, metrics and almost all `messaging.*`
  attributes). Only `error.type`, `server.*` and `network.*` are stable. The *shape* it prescribes
  is useful and stable in spirit: `send` = PRODUCER (the creation context), pull `receive` = CLIENT,
  `process` = CONSUMER, `settle` = CLIENT, and **links are the default correlation mechanism**;
  parenting to the creation context is an opt-in for single-message scenarios only.
- **The link decision matches the most careful prior art.** Eventuous switched to links with an
  explicit rationale (durable trace context + replays = unbounded traces). Axon defaults to a
  separate batch trace (5.x: linked; 4.x: unlinked) and caps same-trace nesting at 2 minutes.
  KurrentDB clients still parent a zero-duration consumer span to the stored context.
- **Storage in prior art:** KurrentDB and Eventuous put `$traceId`/`$spanId` into the event's
  JSON metadata (no flags, no tracestate). Axon puts `traceparent` (+ `tracestate` when present)
  into message metadata. Marten stores no trace context; it derives the optional
  `correlation_id`/`causation_id` columns from the active span. A dedicated column, as decided, is
  cleaner than all of these: it sits outside the payload, so Upcasters and serializers never touch it.
- **Persist `traceparent` only.** Never baggage (W3C: keys and values propagate to other systems;
  keep confidential data out). Our global propagator is `TraceContext + Baggage`, so injection
  must use `propagation.TraceContext{}` directly, not `otel.GetTextMapPropagator()`.
- **No semconv metric exists for consumer lag or backlog.** The only messaging metrics are
  `messaging.client.operation.duration`, `messaging.client.sent.messages`,
  `messaging.client.consumed.messages` and `messaging.process.duration` (all Development).
  Event stores use their own namespaces (`marten.*`, `eventuous.subscription.gap.*`,
  `kafka.consumer_group.lag` in the Collector). Use a library namespace.
- **Multi-pod DB-polled gauges:** OTLP's single-writer rule is satisfied as long as each pod has a
  distinct Resource (`service.instance.id`); the gauges are then N copies of one fact, read with
  `max by (consumer)`. `pkg/otel` currently does not set `service.instance.id`, which a
  Prometheus/Mimir OTLP backend needs for the `instance` label.

## 1. Messaging semconv fit

### Status

The messaging conventions overview, spans and metrics pages are all `Status: Development`.
Instrumentations on v1.24.0 or older SHOULD NOT change their default output until the
conventions are stable and SHOULD offer `OTEL_SEMCONV_STABILITY_OPT_IN=messaging|messaging/dup`
for the migration
([messaging README](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/messaging/README.md)).
In the attribute registry, every `messaging.*` attribute is Development
([registry](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/registry/attributes/messaging.md)).
By contrast the PostgreSQL client span conventions (`db.system.name = "postgresql"`,
`db.query.summary`, `db.collection.name` …) and `db.client.operation.duration` are **Stable**
([postgresql.md](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/db/postgresql.md),
[database-metrics.md](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/db/database-metrics.md)).
`pkg/postgres` already emits those through `otelpgx`.

### Operation types, span kinds and names

From [messaging-spans.md](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/messaging/messaging-spans.md):

| `messaging.operation.type` | Meaning | Span kind |
|---|---|---|
| `create` | One message created; gives a per-message creation context in batch sends | PRODUCER |
| `send` | One or more messages handed to an intermediary | PRODUCER if it is the creation context, else CLIENT |
| `receive` | Pull-based: consumer explicitly requests messages | CLIENT |
| `process` | Messages passed to application code (handler/callback) | CONSUMER |
| `settle` | One or more messages settled (ack/nack) | CLIENT |

- Span name: `{messaging.operation.name} {destination}`, where destination is
  `messaging.destination.template`, else a non-temporary `messaging.destination.name`, else
  omitted when no low-cardinality value exists. Examples: `publish shop.orders`, `process …`.
- Producers SHOULD attach a creation context to each message, "ideally in a way intermediaries
  cannot change"; the mechanism is out of scope.
- For single-message APIs, a `send` span alone is recommended. For batch sends, one `create` per
  message plus `send`, with `create` disable-able.
- `receive` and `process` spans SHOULD link to **each message's** creation context; a span may
  cover a batch or zero messages. Messages that are only prefetched get no span.
- **Links are the default** "because a span has only one parent". Parent-child is an opt-in only
  for single-message `process`. When processing runs inside another ambient span, the default is a
  child of that ambient span plus a link to the creation context.
- In a batch, attribute values shared by all messages go on the batch span; values that differ go
  on the individual links.

### Required attributes and `messaging.system`

Required on every span and metric: `messaging.system`, `messaging.operation.name`. Conditionally
required: `messaging.operation.type`, `messaging.destination.name`/`.template`,
`messaging.batch.message_count` (batches only, never on single-message spans), `error.type` (only
on failure). Recommended: `messaging.message.id`, `messaging.message.conversation_id` ("sometimes
called Correlation ID"), `messaging.consumer.group.name`, `messaging.client.id`.

`messaging.system` has a closed list of well-known values (kafka, rabbitmq, pulsar, servicebus,
gcp_pubsub, aws_sqs, …); "if one applies it MUST be used, otherwise a custom value MAY be used".
None applies to a Postgres event log.

### What prior art does

| Project | Append | Consumer | Attribute namespace |
|---|---|---|---|
| KurrentDB clients | PRODUCER `streams.append` | CONSUMER `streams.subscribe`, one per received event, **child** of stored context, zero duration (started and disposed immediately, does not wrap the handler) | `db.system = "kurrentdb"`, `db.kurrentdb.stream`, `db.kurrentdb.event.type`, `db.kurrentdb.subscription.id` |
| Eventuous | command service span INTERNAL; producer span PRODUCER | consume span CONSUMER, **linked root** when only stored context exists; handler spans INTERNAL children | own tags plus `messaging.message.id`, `messaging.conversation_id` |
| Axon 4.13 | `${EventBus}.publish(${Event})`, `${EventBus}.commit` | `${ProcessorType}[${name}].batch` root, per-handler child spans | `axon_*` |
| Axon 5.3 | dispatch spans PRODUCER | handler spans CONSUMER; default streaming topology is a separate batch trace **linked** to the publication trace | |
| Marten | `marten.connection` (opt-in) | per shard `marten.{projection}.all` with `execution`/`loading`/`grouping` spans per page of events, tagged with sequence floor/ceiling | `marten.*` |

Sources: KurrentDB
[ActivitySourceExtensions.cs](https://github.com/kurrent-io/KurrentDB-Client-Dotnet/blob/master/src/KurrentDB.Client/Core/Common/Diagnostics/ActivitySourceExtensions.cs),
[Python client observability](https://docs.kurrent.io/clients/python/v1.3/observability);
Eventuous
[SubscriptionActivity.cs](https://github.com/Eventuous/eventuous/blob/dev/src/Core/src/Eventuous.Subscriptions/Diagnostics/SubscriptionActivity.cs);
Axon [4.13 tracing](https://docs.axoniq.io/axon-framework-reference/4.13/monitoring/tracing/),
[5.3 OpenTelemetry](https://docs.axoniq.io/axon-framework-reference/5.3/tracing/opentelemetry/);
Marten [async daemon](https://martendb.io/events/projections/async-daemon.html),
[OpenTelemetry](https://martendb.io/otel.html).

Eventuous documents the reason for links in the code (`GetParentOrLink`):

> The trace context restored from message metadata is durable data on the message, so it outlives
> the process that produced it. Parenting to it would put every redelivery of that message — a
> replay, a checkpoint reset, a hot resubscribe loop — into the same trace, forever […]. A link
> records the same causality without joining the trace, which is also what the OpenTelemetry
> messaging conventions prescribe for an asynchronous "process" operation.

Axon 4.13 keeps `distributedInSameTrace` off by default "to keep traces from spreading over
arbitrarily long time spans", and even when enabled limits it with
`distributedInSameTraceTimeLimit` (default `PT2M`).

Eventuous also shows a Go-relevant detail: a linked span must be started as an explicit **new
root**, otherwise an unrelated ambient span (or a stale one on the consumer goroutine) becomes the
parent and takes the sampling decision. In Go this is `trace.WithNewRoot()` plus `trace.WithLinks`.

### Links and sampling

The SDK passes links to `Sampler.ShouldSample`, but no built-in sampler uses them; a span without
a parent goes to `ParentBased`'s `root` sampler
([trace SDK spec](https://opentelemetry.io/docs/specs/otel/trace/sdk/)). So a linked-root Consumer
span takes a **fresh** head-sampling decision; the stored sampled flag does not carry over. Links
must be passed at span start (`trace.WithLinks`) to be visible to samplers. The Go SDK keeps at
most 128 links per span by default (`OTEL_SPAN_LINK_COUNT_LIMIT`).

## 2. Trace context storage in event stores

| Project | Where | What | Notes |
|---|---|---|---|
| KurrentDB (.NET, Python, Node clients) | event metadata JSON | `$traceId`, `$spanId` | Only injected when a listener is active; merges into user JSON and skips if keys exist; no trace flags, no tracestate. ([TracingConstants.cs](https://github.com/kurrent-io/KurrentDB-Client-Dotnet/blob/master/src/KurrentDB.Client/Core/Common/Diagnostics/Core/Tracing/TracingConstants.cs), [EventMetadataExtensions.cs](https://github.com/kurrent-io/KurrentDB-Client-Dotnet/blob/master/src/KurrentDB.Client/Core/Common/Diagnostics/EventMetadataExtensions.cs)) |
| Eventuous | message metadata | `$traceId`, `$spanId` (same keys as KurrentDB) | `TracingMeta(TraceId, SpanId)`; ([MetaTags.cs](https://github.com/Eventuous/eventuous/blob/dev/src/Core/src/Eventuous.Diagnostics/Tags/MetaTags.cs)) |
| Axon | message metadata | `traceparent`, plus `tracestate` when present | Injected via the configured propagator at dispatch time ([5.3 docs](https://docs.axoniq.io/axon-framework-reference/5.3/tracing/opentelemetry/)) |
| Marten | opt-in columns `correlation_id`, `causation_id`, `headers`, user name | no trace context | "The CorrelationId and CausationId is taken automatically from any active OpenTelemetry span"; columns are only created on opt-in ([metadata](https://martendb.io/events/metadata.html)) |

Observation: nobody persists baggage, and only Axon persists `tracestate`. The KurrentDB/Eventuous
format drops trace flags, so a consumer cannot tell whether the producer span was sampled.

### W3C rules that matter for persistence

- **Trace Context** is a W3C Recommendation (2021). `traceparent` =
  `version-traceid-parentid-traceflags`; the sampled bit means the caller *may* have recorded
  data and is a recommendation only. `tracestate` holds up to 32 vendor entries; vendors SHOULD
  propagate at least 512 characters. The fields exist "only to enable trace correlation", must
  not carry PII, and IDs must not be derived from user data
  ([trace-context](https://www.w3.org/TR/trace-context/)).
- **Baggage** is a Candidate Recommendation Snapshot (2024), up to 64 entries / 8192 bytes.
  Security: make sure "no proprietary or confidential information is stored in baggage".
  Privacy: "keys and values can be propagated to other systems"
  ([baggage](https://www.w3.org/TR/baggage/)).

### Risks of persisting forever and replaying

1. **Unbounded traces on replay/rebuild.** Parenting puts every replay, Cursor reset and rebuild
   into the original trace (Eventuous, Axon rationale above). Links avoid it; already decided.
2. **Span floods during rebuilds.** A Projection rebuild re-processes the whole log. With
   per-event spans plus one link per event, a rebuild emits millions of spans. Axon offers
   `disableBatchTrace`; Marten traces per page of events, not per event.
3. **Sampling.** The stored flag reflects a decision made at append time, possibly by another
   service. A linked root decides afresh; links to unsampled spans dangle. Trace backends also
   expire data in days while events live forever, so old links dangle anyway. Both are harmless
   but worth documenting.
4. **Upcasting and serialization.** Formats that put trace IDs in the metadata JSON must merge
   them into user JSON (KurrentDB code does exactly that). A dedicated column outside the
   payload is untouched by Upcasters, which are strictly one event in, one event out.
5. **Baggage leaks.** Persisting baggage would freeze arbitrary, possibly confidential
   request-scoped key/values into an immutable log, and replay them into every Reactor and Inbox
   handler. Our `pkg/otel` installs `TraceContext{}` + `Baggage{}` as the global propagator, so
   calling `otel.GetTextMapPropagator().Inject` would persist baggage. Use
   `propagation.TraceContext{}` explicitly.
6. **tracestate.** Vendor-specific, up to 32 entries, and only meaningful for the live request.
   Low value in a durable log; Axon stores it, KurrentDB/Eventuous do not.
7. **PII.** `traceparent` holds random IDs only (W3C), so it is safe in an immutable, never-deleted
   table. Correlation and causation already live in the Envelope, which removes any need for
   baggage.

## 3. Metric naming

### What semconv offers

[messaging-metrics.md](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/messaging/messaging-metrics.md)
(all Development):

| Metric | Instrument | Unit |
|---|---|---|
| `messaging.client.operation.duration` | Histogram | `s` (not for processing time) |
| `messaging.client.sent.messages` | Counter | `{message}` |
| `messaging.client.consumed.messages` | Counter | `{message}` (counted at receipt) |
| `messaging.process.duration` | Histogram | `s` (MUST for `process` operations) |

All require `messaging.system` and `messaging.operation.name`. **There is no lag, backlog or
age metric.** The Collector's Kafka metrics receiver uses its own names: `kafka.consumer_group.lag`
and `kafka.consumer_group.lag_sum` (Gauge, unit `1`, attributes group/topic/partition)
([kafkametricsreceiver](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/receiver/kafkametricsreceiver/documentation.md)).

### Prior art in event stores

- **Marten**: `marten.{projection}.all.processed` (counter), `.gap` (histogram, distance between
  the high-water mark and the shard's progress), `.skipped` (counter), `marten.daemon.skipping`,
  `marten.event.append` (counter, tags `event_type`, `tenant.id`),
  `marten.fetch_for_writing.events_replayed` (histogram, tags `aggregate.type`, `fetch.plan`).
  The projection name inside the metric name is an anti-pattern under OTel naming.
- **Eventuous**: `eventuous.subscription.gap.count` (ObservableGauge, unit `events`),
  `eventuous.subscription.gap.seconds` (ObservableGauge, `s`),
  `eventuous.subscription.pendingCheckpoint.count`, `eventuous.subscription.duration`
  (Histogram, `ms`), `eventuous.subscription.errors.count` (Counter); tag `subscription-id`
  ([SubscriptionMetrics.cs](https://github.com/Eventuous/eventuous/blob/dev/src/Core/src/Eventuous.Subscriptions/Diagnostics/SubscriptionMetrics.cs)).
  Both lag in events and lag in seconds, as observable gauges.

### Naming rules that apply

From [naming.md](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/general/naming.md)
and [metrics.md](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/general/metrics.md):

- Custom names: prefix with a company reverse domain, or with an application name that is unique
  within the organization; do **not** prefix with an existing OTel namespace (`messaging.`,
  `db.`); `otel.*` is reserved.
- Units in UCUM, not in the name: `s` for durations, `{event}` / `{message}` annotations for
  counts (singular), `By` for bytes.
- Namespaces are not pluralized; a name is pluralized only when its unit is an annotation
  (`… .events`); UpDownCounters are not pluralized; use `.count` for "number of items";
  `.duration` for histograms of discrete operations; no `_total` suffix on counters or
  UpDownCounters (side note: `pkg/nats` currently registers `nats.reconnects.total`).
- Asynchronous instruments are equally compliant; the choice is an implementation detail.

### Multi-pod duplicate gauges

The OTLP data model requires "one logical writer" per metric stream, and fixes duplicates by
"differentiating the Resource used". Gauges "do not provide an aggregation semantic" and cannot
be assumed to be sums
([data model](https://opentelemetry.io/docs/specs/otel/metrics/data-model/)). Prometheus' OTLP
ingestion maps `service.instance.id` to `instance` and `service.name` to `job`; other resource
attributes such as `host.name` only land on `target_info`
([Prometheus OTel guide](https://prometheus.io/docs/guides/opentelemetry/)).

Practices found:

- **Every pod reports, query with `max`.** Distinct `service.instance.id` makes N valid streams of
  the same fact; dashboards and alerts use `max by (consumer)` (never `sum`). Robust: the metric
  survives when the pod running a Consumer dies, which is exactly when lag matters.
- **Only the leader reports.** Marten runs each projection on one node via a Postgres advisory
  lock (HotCold mode), so its per-shard metrics come from the lock holder. This avoids
  duplicates, but the series disappears with the leader; alerts need `absent()`.
- **Outside the app.** A single Collector with the `sqlquery` receiver (metrics: alpha) or a
  Postgres exporter queries the tables. One writer by construction, but outside the library.

## Recommendation for go-utils

### Tracing

Tracer and meter names follow `pkg/nats`: the package import path, e.g.
`github.com/deepworx/go-utils/pkg/eventsourcing/consumer`.

Do not set `messaging.system` and do not claim messaging-semconv conformance. The conventions are
Development, none of the well-known systems applies, the required attribute set
(`messaging.destination.name`, …) does not map cleanly to a Postgres log, and the SQL is already
described by stable `db.*` spans from `otelpgx`. Do adopt the semconv **shape**: span kinds,
`{operation} {destination}` names and links. Library attributes go under an `eventsourcing.*`
namespace (internal application prefix, allowed by naming.md) plus stable `error.type`.

| Component | Span | Kind | Parent / links | Attributes |
|---|---|---|---|---|
| Command transaction | `{command} {stream type}` around load + decide + append + commit | INTERNAL | child of the caller's span (RPC handler or Reactor) | `eventsourcing.stream.type`, `eventsourcing.tenant.id`, `eventsourcing.events.count` (appended), `eventsourcing.stream.version`, `error.type` (e.g. `concurrency_conflict`, `expected_version_mismatch`) |
| Append | `send {stream type}`, child of the Command transaction span, only when events are written | PRODUCER | its span context is the **creation context** written to `events.trace_context` | `eventsourcing.events.count` |
| Consumer batch | `process {consumer name}` | CONSUMER | `trace.WithNewRoot()`; links to each event's stored context, up to the SDK link limit (128 default), each link carrying `eventsourcing.event.position` | `eventsourcing.consumer.name`, `eventsourcing.events.count`, cursor from/to positions |
| Per-event handle | `handle {event type}` | INTERNAL | child of the batch span; one link to that event's stored context | `eventsourcing.event.type`, `eventsourcing.event.id`, `eventsourcing.stream.type`, `eventsourcing.event.position`, `eventsourcing.correlation.id` |
| Reactor | uses the per-event handle span as the ambient context; the commands it issues create Command transaction spans as **children** | (inherits) | causal chain across hops = links from each new event back to the one that triggered it | `eventsourcing.causation.id` |
| Inbox processing | `process {inbox name}` per leased message | CONSUMER | new root, link to the stored `traceparent` of the receiving request | `eventsourcing.inbox.name`, `eventsourcing.inbox.message.id`, attempt number, `error.type` |

Rules:

- **Write path:** inject with `propagation.TraceContext{}` into a one-key carrier and store the
  `traceparent` string only if `SpanContext().IsValid()`; otherwise NULL. Never the global
  propagator, never baggage. Do not store `tracestate` (the column stays a single `traceparent`).
- **Read path:** parse with `propagation.TraceContext{}.Extract`, take
  `trace.SpanContextFromContext`, pass `trace.Link{SpanContext: sc}` via `trace.WithLinks` at span
  start. Ignore unparsable values silently (they are telemetry, not data).
- **Rebuilds/replays:** offer a Consumer option that turns off per-event spans and links (batch
  span only, comparable to Marten's per-page spans). Enable it by default for rebuilds.
- **No `receive` or `settle` spans:** fetching the batch and advancing the Cursor or settling the
  Inbox lease happen in the batch transaction and are already visible as `db.*` child spans.

### Metrics

Meter per package, names under `eventsourcing.*` (and `inbox.*` if the Inbox stays a separate
package, matching `nats.*`). Histograms use seconds and the semconv advisory buckets for messaging
durations (0.005 s to 10 s).

| Metric | Instrument | Unit | Attributes | Source |
|---|---|---|---|---|
| `eventsourcing.command.duration` | Float64Histogram | `s` | `eventsourcing.stream.type`, `error.type` | Command transaction |
| `eventsourcing.appended.events` | Int64Counter | `{event}` | `eventsourcing.stream.type` | append |
| `eventsourcing.aggregate.load.duration` | Float64Histogram | `s` | `eventsourcing.stream.type` | aggregate replay |
| `eventsourcing.aggregate.load.events` | Int64Histogram | `{event}` | `eventsourcing.stream.type` | events replayed per load (Marten's `events_replayed`) |
| `eventsourcing.consumer.batch.duration` | Float64Histogram | `s` | `eventsourcing.consumer.name`, `error.type` | batch |
| `eventsourcing.consumer.process.duration` | Float64Histogram | `s` | `eventsourcing.consumer.name`, `eventsourcing.event.type`, `error.type` | per-event handle (mirrors `messaging.process.duration`) |
| `eventsourcing.consumer.processed.events` | Int64Counter | `{event}` | `eventsourcing.consumer.name`, `error.type` | per-event handle |
| `eventsourcing.consumer.backlog` | Int64ObservableGauge | `{event}` | `eventsourcing.consumer.name` | DB poll: events after the Cursor and below the visibility horizon |
| `eventsourcing.consumer.lag` | Float64ObservableGauge | `s` | `eventsourcing.consumer.name` | DB poll: age of the oldest unprocessed event, 0 when caught up |
| `eventsourcing.parked_event.count` | Int64ObservableGauge | `{event}` | `eventsourcing.consumer.name` | DB poll |
| `eventsourcing.xmin.age` | Float64ObservableGauge | `s` | none | DB poll: age of the oldest open transaction holding back the `xid8` visibility horizon |
| `inbox.message.count` | Int64ObservableGauge | `{message}` | `inbox.name`, `inbox.message.state` (`pending`, `leased`, `failed`) | DB poll |
| `inbox.lag` | Float64ObservableGauge | `s` | `inbox.name` | DB poll: age of the oldest pending message |
| `inbox.process.duration` | Float64Histogram | `s` | `inbox.name`, `error.type` | Inbox processing |

Cardinality: never put tenant, stream ID, event ID or positions on metrics; those belong on spans.

### Multi-pod gauge handling

- Every pod reports the DB-polled gauges by default, computed by **one** aggregated query per
  collection cycle per package (all Consumers in one round trip), so N pods cost N cheap queries
  per interval.
- Document that these are global facts: aggregate with `max by (eventsourcing_consumer_name)`,
  never `sum`.
- Offer an option to disable the DB-polled gauges so a deployment can pick one reporting
  workload.
- Follow-up in `pkg/otel`: set `service.instance.id` on the Resource (e.g. pod name or a UUID).
  Without it, OTLP-to-Prometheus backends merge the pods into one `instance` and the gauges become
  the multi-writer error condition the data model describes.

## Sources

- OpenTelemetry semantic conventions (v1.44.0 / `main`):
  [messaging README](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/messaging/README.md),
  [messaging spans](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/messaging/messaging-spans.md),
  [messaging metrics](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/messaging/messaging-metrics.md),
  [messaging attribute registry](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/registry/attributes/messaging.md),
  [PostgreSQL](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/db/postgresql.md),
  [database metrics](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/db/database-metrics.md),
  [naming](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/general/naming.md),
  [metrics guidelines](https://github.com/open-telemetry/semantic-conventions/blob/main/docs/general/metrics.md)
- OpenTelemetry specification:
  [metrics data model](https://opentelemetry.io/docs/specs/otel/metrics/data-model/),
  [trace SDK](https://opentelemetry.io/docs/specs/otel/trace/sdk/)
- OpenTelemetry Collector contrib:
  [kafkametricsreceiver](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/receiver/kafkametricsreceiver/documentation.md),
  [sqlqueryreceiver](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/receiver/sqlqueryreceiver/README.md)
- W3C: [Trace Context](https://www.w3.org/TR/trace-context/), [Baggage](https://www.w3.org/TR/baggage/)
- Prometheus: [Using Prometheus as your OpenTelemetry backend](https://prometheus.io/docs/guides/opentelemetry/)
- KurrentDB: [.NET client source](https://github.com/kurrent-io/KurrentDB-Client-Dotnet),
  [Python client observability](https://docs.kurrent.io/clients/python/v1.3/observability)
- Eventuous: [source](https://github.com/Eventuous/eventuous)
- Axon Framework: [4.13 tracing](https://docs.axoniq.io/axon-framework-reference/4.13/monitoring/tracing/),
  [5.3 OpenTelemetry](https://docs.axoniq.io/axon-framework-reference/5.3/tracing/opentelemetry/)
- Marten: [OpenTelemetry](https://martendb.io/otel.html),
  [event metadata](https://martendb.io/events/metadata.html),
  [async daemon](https://martendb.io/events/projections/async-daemon.html)
