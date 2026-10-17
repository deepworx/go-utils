// Package projection runs versioned read models (ADR-0005).
//
// A Projection version N keeps its read model in its own tables, by
// convention <table>_vN, and is built by the Consumer <name>@vN. Readers
// query a view <table> that the service's own migrations point at the
// active version; the library never runs DDL. During a version bump a
// newer version builds as a shadow beside the active one:
//
//	p, err := projection.New(rt, projection.Config{
//		Name:   "tasks",
//		Active: projection.Version{N: 1, Handler: tasksV1},
//		Shadow: &projection.Version{N: 2, Handler: tasksV2},
//	})
//	err = consumer.Group(p.Consumers()).Run(ctx)
//
// Each version's Consumer runs Transactional, so its writes through
// postgres.Conn (postgres.Stage in memory) commit with its Cursor. Await
// waits for the active version only; CaughtUp tells whether the shadow (or
// the active version, without a shadow) has caught up.
//
// # Change notifications
//
// A handler of the active version calls Notify(ctx, entity, id) for every
// entity it changed. The Change {Tenant, Entity, ID} is sent inside the
// Cursor transaction, so Subscriptions see it only after the commit and
// never for a failed transaction. In the shadow, Notify does nothing. A
// PgRuntime sends it on the channel <schema>.changes, and Subscribe
// receives it through the runtime's notify.Listener; an InMemoryRuntime
// delivers it in process. A Change with Gap set means changes may have been
// lost: refetch everything shown.
//
// # Version bump
//
// Changing a read model's shape, or repairing its data, is a version bump.
// It spans two releases plus a cleanup:
//
//  1. Release N: a migration creates <table>_vN+1, and the code registers
//     version N+1 as the Shadow. It builds from the first event while
//     readers keep using version N, which stays live.
//  2. Release N+1, once CaughtUp reports true: a migration switches the
//     view, guarded by a DO block that raises while the shadow lags by
//     more than a threshold (see below). The code makes N+1 the Active
//     version and drops version N.
//  3. A later release drops <table>_vN and its Cursor row
//     (DELETE FROM public.consumer_cursors WHERE name = '<name>@vN').
//
// The migration of release N+1 for a Projection "tasks" going to version 2:
//
//	DO $$
//	BEGIN
//	    IF public.consumer_pending('tasks@v2', 100) > 100 THEN
//	        RAISE EXCEPTION 'projection tasks@v2 is not caught up';
//	    END IF;
//	END
//	$$;
//	CREATE OR REPLACE VIEW tasks AS SELECT id, title, done FROM tasks_v2;
//
// The threshold (100 here) bounds how many events the new active version
// may still have to process when the view switches. consumer_pending
// raises by itself while the shadow is Stopped or has no Cursor. CREATE OR
// REPLACE VIEW only accepts a view that keeps every column with its type,
// which pods of release N still read during the rollout; removing or
// retyping a column takes a later bump.
//
// Reset clears every version and rewinds its Cursor in one transaction. It
// is for prototypes and development only.
package projection
