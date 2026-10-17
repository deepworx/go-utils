// Package consumer runs Consumers: named, Cursor-tracked readers of the
// event log.
//
// A Consumer reads the events matching its event-type filter in Cursor
// order and hands each one to its Handler, usually a Router built from typed
// handlers:
//
//	r := consumer.NewRouter(codec)
//	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, e TaskOpened) error {
//		return q.InsertTask(ctx, postgres.Conn(ctx, pool), env.StreamID, e.Title)
//	})
//	tasks, err := consumer.New(rt, consumer.Config{
//		Name:    "task_list",
//		Mode:    consumer.Transactional,
//		Handler: r,
//	})
//
// In Transactional mode the handler's writes and the Cursor commit in one
// Cursor transaction, so every event takes effect exactly once. In
// OwnTransaction mode the Cursor advances after each event, so delivery is
// at least once.
//
// A failing event is retried with backoff and then the Consumer goes
// Stopped; it never skips an event. Permanent errors, panics and decode
// errors stop it at once. A Stopped Consumer stays at its Cursor until
// Runtime.Resume.
//
// The filter is stored on the Cursor and checked when a release changes it
// (ADR-0006). Removed types are always accepted. An OwnTransaction Consumer
// handles added types from its Cursor onwards. A Transactional Consumer
// accepts added types only when no event of them lies at or below its
// Cursor; otherwise it goes Stopped, and the change needs a new Projection
// version. Resume does not skip this check.
//
// The Runtime keeps the Cursors. NewPgRuntime runs on an eventstore.PgStore
// with Cursors in the consumer_cursors table; handlers write through
// postgres.Conn, which joins the Cursor transaction. Each Consumer runs under
// a lease, so only one process at a time processes it, and wakes up on
// NOTIFY when a Listener is given. NewInMemoryRuntime runs on an
// eventstore.InMemoryStore; its handlers register writes with
// postgres.Stage so they roll back with the Cursor transaction.
//
// Consumers only see events below pg_snapshot_xmin (or the in-memory
// equivalent), so an event of a transaction that commits late is never
// skipped. Any open transaction that holds a transaction ID, in any
// database of the cluster, delays visibility until it ends.
//
// Run all Consumers of a process with a Group:
//
//	err := consumer.Group{tasks, mailer}.Run(ctx)
//
// Tests and one-shot jobs call Drain, which processes synchronously up to
// the head of the log at the time of the call.
package consumer
