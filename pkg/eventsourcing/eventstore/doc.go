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
//
// A Repository runs each command in a Command transaction on one Stream:
//
//	tasks := eventstore.NewRepository[TaskID, Task](eventstore.RepositoryConfig{
//		Store:      eventstore.NewInMemoryStore(codec),
//		UnitOfWork: eventstore.NewInMemoryUnitOfWork(),
//	})
//	res, err := tasks.Update(ctx, id, func(ctx context.Context, t *Task) error {
//		return t.Complete()
//	}, eventstore.ExpectVersion(lastVersion))
//
// The Tenant comes from ctxutil.TenantID. Each event records its Actor
// (ctxutil.WithActor, else Claims.UserID), Correlation
// (ctxutil.WithCorrelationID, else the request ID, else a UUIDv7 per Command
// transaction) and Causation (ctxutil.WithCausationID), as ADR-0010 defines.
package eventstore
