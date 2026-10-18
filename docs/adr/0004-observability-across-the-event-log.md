# Observability across the event log

The `send` span that appends events stores its W3C `traceparent` in a nullable `trace_context` column on `events` and on the inbox. A Consumer's `process` span is a new root that **links** to those stored contexts instead of becoming their child. A batch mixes events from many origins and can run hours later, so parenting would produce many-hour traces and attach each batch to an arbitrary one of them. Gauges polled from the database (pending, lag, parked events, stopped) are reported only by the pod that holds the Consumer's lock, so each series has exactly one writer.

## Considered Options

- **Correlation through `correlation_id` only, no stored trace context.** This was rejected because following a request through the Reactor hops it triggers is the debugging case event sourcing makes hardest, and the column is cheap while the schema is still ours to shape.
- **Consumer spans as children of the stored context** (as the KurrentDB clients do). This was rejected for the batch and latency reasons above. The OTel messaging conventions, Eventuous and Axon 5 also prefer links.
- **Every pod reports the polled gauges, read with `max by`.** This is the common practice and what the research recommended. It was rejected because it relies on every dashboard and alert aggregating correctly, and duplicate series from pods that share an identity break single-writer backends.

## Consequences

- Only `traceparent` is stored, injected with `propagation.TraceContext{}` directly. The global propagator would also persist baggage. `tracestate` is not stored.
- A linked root makes its own sampling decision. Old links can point to traces the backend has already expired, which is harmless.
- Rebuilds can turn off per-event spans and links, so a replay of millions of events doesn't produce millions of spans.
- When lock ownership moves between pods, a gauge series goes stale on one pod and reappears on another, with a short gap. A Stopped Consumer keeps its lock, so it keeps reporting `stopped=1`.
- `pkg/otel` must set `service.instance.id`, or pods collapse into one series identity.
- `xmin` age is not per Consumer. Each process reports it once.
