# go-utils

Shared Go building blocks for deepworx services, including Postgres-only event sourcing.

## Event sourcing

**Tenant**:
The customer organisation that owns a piece of data; every Stream, guard and identifier range belongs to exactly one Tenant.
_Avoid_: Organisation, account, customer

**Aggregate**:
A consistency boundary whose state is rebuilt from the events of exactly one Stream.
_Avoid_: Entity, model

**Stream**:
The ordered sequence of events belonging to one Aggregate instance, identified by a UUID that is unique within its Tenant; its stream type names the kind of Aggregate.

**Command transaction**:
The transaction that changes exactly one Aggregate's Stream; writers to the same Stream are serialized from before the load until commit.
_Avoid_: Unit of work (names the mechanism, not the concept)

**Expected version**:
The Aggregate version a client last saw, sent with a command so the command fails instead of acting on a stale view.

**Command ID**:
A client-chosen UUID that makes a command take effect at most once per Tenant.
_Avoid_: Idempotency key, request ID

**Event type**:
The permanent name of a kind of event, stored with every event of that kind; it never changes once events have been written.

**Schema version**:
The version of an event type's stored shape. It rises by one with every Upcaster added for that event type.

**Upcaster**:
A step that lifts one stored event from one Schema version to the next, strictly one event in and one event out.
_Avoid_: Migration (reserved for database schema changes)

**Envelope**:
An event together with its log metadata (stream, positions, actor, correlation, causation).
_Avoid_: StoredEvent, record

**Actor**:
Who caused an event: a user, or a library component acting on its own (a Reactor, the scheduler, an Inbox Source).
_Avoid_: User, author, occurred by

**Correlation**:
The shared origin of every event that one request, scheduled run or Inbox message led to, directly or through Reactors.
_Avoid_: Trace (the OpenTelemetry concept), request ID

**Causation**:
The event that directly caused another event, through a Reactor. Events caused by a user, the scheduler or an Inbox message have none.
_Avoid_: Parent, trigger

**Cursor**:
A position in the event log up to which a Consumer has processed events.
_Avoid_: Checkpoint, offset

**Cursor transaction**:
The transaction in which a Consumer writes its effect and its Cursor together, so an event takes effect exactly once; the counterpart to the Command transaction.
_Avoid_: Unit of work

**Consumer**:
A named, Cursor-tracked reader of the event log. Not to be confused with a NATS consumer, which reads a JetStream stream.
_Avoid_: Subscriber, handler

**Stopped**:
The state of a Consumer that has used up its retries and halted at its Cursor until it is restarted.
_Avoid_: Dead, failed, crashed

**Projection**:
A Consumer that builds a queryable read model from events.
_Avoid_: View, read side

**Projection version**:
One numbered generation of a Projection's read model, with its own tables and its own Consumer. Readers see exactly one version, the active one; a newer version may build beside it as a shadow until it takes over. Changing a Projection's shape or repairing its data means starting a new version.
_Avoid_: Rebuild (a rebuild is just a new version catching up)

**Reactor**:
A stateless Consumer that turns events into commands; any state it needs lives in Aggregates.
_Avoid_: Process manager (reserved for a stateful, per-instance coordinator), policy, saga

**Parked**:
Held back from processing until a redrive succeeds or an Operator discards it. For a Reactor, a Stream is parked: the Reactor holds the failed event and every later event of that Stream while its Cursor moves on for all other Streams. For an Inbox, a single message is parked once its retries are spent. Unlike Stopped, it confines a failure to one Stream or one message.
_Avoid_: Dead-lettered, quarantined

**Redrive**:
Releasing something Parked for another attempt. A Reactor replays a Parked Stream's held events in order, automatically on a schedule or by an Operator, stopping at the first failure and never touching the Cursor; an Inbox message is redriven only by an Operator, with its attempts reset.
_Avoid_: Retry (the attempts before something is parked)

**Operator**:
A trusted caller who works across all Tenants to keep Consumers and Inboxes running: listing, Redriving and discarding Parked work, and watching changes. The library does not authorize Operators; an app that exposes operator calls to a tenant admin guards them itself (ADR-0014).
_Avoid_: Admin (suggests a tenant admin), support user

**Business date**:
A calendar date in the service's business timezone; a daily scheduled job runs at most once per business date, and missed dates are never run retroactively.
_Avoid_: Run date, day

**Inbox**:
A durable, leased work queue for incoming external messages (e.g. webhooks), independent of the event log. A message may belong to a Tenant or to none; Operators see the messages of every Tenant.
_Avoid_: Webhook inbox

**Source**:
The named origin of Inbox messages (e.g. one webhook provider); a message's key is unique within its Source, and each Source has its own handler.
_Avoid_: Provider, channel, topic
