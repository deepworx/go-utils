// Package reactor turns events into commands (ADR-0007).
//
// A Reactor is a stateless Consumer: any state it needs lives in
// Aggregates, and its handler issues commands through the app's own
// Repository. Run it in a consumer.Group:
//
//	r, err := reactor.New(rt, reactor.Config{
//		Name:    "shipping",
//		Handler: router, // a *consumer.Router
//		StartAt: reactor.FromNow,
//	})
//	group := consumer.Group{r.Consumer(), ...}
//
// The handler ctx carries the event's Tenant and Correlation, the Actor
// "system:reactor:<name>" and the event as Causation, so the commands it
// issues record where they came from.
//
// Delivery is at least once. Use env.EventID as the idempotency key of
// outbound calls; commands rely on Aggregate state guards.
//
// A reaction that fails after its retries, or at once with
// consumer.Permanent, a panic or an undecodable event, Parks its Stream.
// The Reactor holds the event and every later event of that Stream in order
// while its Cursor moves on for all other Streams. Redrive replays a Parked
// Stream, Discard skips one held event and Parked lists them. In Postgres
// the park list lives in the parked_streams and parked_events tables of
// docs/postgres/schema.sql.
package reactor
