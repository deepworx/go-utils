// Package eventstore stores domain events in the event log.
//
// Events are stored as JSON of the domain event struct itself (ADR-0001).
// Each event type is registered on a Codec together with its Upcasters; its
// Schema version is the number of Upcasters plus one:
//
//	codec := eventstore.NewCodec()
//	eventstore.Register[TaskCreated](codec)
//	eventstore.Register[TaskRenamed](codec, eventstore.UpcastObject(func(m map[string]any) error {
//		m["title"] = m["name"]
//		delete(m, "name")
//		return nil
//	}))
//
// A service test calls CheckCodec(t, codec) to catch missing json tags and
// lossy round trips before they reach production.
package eventstore
