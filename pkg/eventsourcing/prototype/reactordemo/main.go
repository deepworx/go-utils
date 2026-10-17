// PROTOTYPE (DWX-221): walkthrough of the Reactor API.
//
//	go run ./pkg/eventsourcing/prototype/reactordemo
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/reactor"
	"github.com/deepworx/go-utils/pkg/eventsourcing/prototype/todo"
)

// CRM is the external system the Reactor drives. Failures are scripted.
type CRM struct {
	down    bool
	reject  map[string]bool // title → 422 (Permanent)
	broken  map[string]bool // title → 503 (transient) until cleared
	flaky   int             // fail the next n calls
	calls   []string
	nextRef int
}

var ErrCRMDown = errors.New("crm: 503")

func (c *CRM) call(op string) error {
	c.calls = append(c.calls, op)
	if c.down {
		return ErrCRMDown
	}
	if c.flaky > 0 {
		c.flaky--
		return ErrCRMDown
	}
	return nil
}

func (c *CRM) Upsert(id uuid.UUID, title string) error {
	if c.reject[title] {
		c.calls = append(c.calls, "upsert "+title+" → 422")
		return consumer.Permanent(fmt.Errorf("crm: 422 invalid title %q", title))
	}
	if c.broken[title] {
		c.calls = append(c.calls, "upsert "+title+" → 503")
		return ErrCRMDown
	}
	return c.call(fmt.Sprintf("upsert %s %q", id.String()[28:], title))
}

func (c *CRM) Close(id uuid.UUID) (string, error) {
	if err := c.call("close " + id.String()[28:]); err != nil {
		return "", err
	}
	c.nextRef++
	return fmt.Sprintf("deal-%d", c.nextRef), nil
}

type Tasks = eventstore.Repository[todo.TaskID, todo.Task, *todo.Task]

// crmSync is the app's Reactor definition: what a service writes.
func crmSync(store *eventstore.InMemoryStore, tasks *Tasks, crm *CRM) consumer.Handler {
	r := consumer.NewRouter(store)
	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, e todo.TaskOpened) error {
		return crm.Upsert(env.StreamID, e.Title)
	})
	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, e todo.TaskRenamed) error {
		return crm.Upsert(env.StreamID, e.Title)
	})
	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, e todo.TaskCompleted) error {
		ref, err := crm.Close(env.StreamID)
		if err != nil {
			return err
		}
		// The command: ctx already carries tenant, actor, causation, correlation.
		_, err = tasks.Update(ctx, todo.TaskID(env.StreamID), func(_ context.Context, t *todo.Task) error {
			return t.Archive(ref)
		})
		if errors.Is(err, todo.ErrArchived) {
			return nil // idempotent repeat: the aggregate already reacted
		}
		return err
	})
	return r
}

func main() {
	ctx := context.Background()
	codec := eventstore.NewCodec()
	eventstore.Register[todo.TaskOpened](codec)
	eventstore.Register[todo.TaskRenamed](codec)
	eventstore.Register[todo.TaskCompleted](codec)
	eventstore.Register[todo.TaskArchived](codec)
	store := eventstore.NewInMemoryStore(codec)
	uow := eventstore.NewInMemoryUnitOfWork()
	tasks := eventstore.NewRepository[todo.TaskID, todo.Task](eventstore.RepositoryConfig{Store: store, UnitOfWork: uow})
	rt := consumer.NewInMemoryRuntime(store)
	crm := &CRM{reject: map[string]bool{}, broken: map[string]bool{}}

	acme := eventstore.WithTenant(ctx, "acme")
	globex := eventstore.WithTenant(ctx, "globex")
	newID := func() todo.TaskID { return todo.TaskID(uuid.Must(uuid.NewV7())) }
	open := func(c context.Context, id todo.TaskID, title string) {
		must(tasks.Create(c, id, func(_ context.Context, t *todo.Task) error { return t.Open(title) }))
	}
	rename := func(c context.Context, id todo.TaskID, title string) {
		must(tasks.Update(c, id, func(_ context.Context, t *todo.Task) error { return t.Rename(title) }))
	}
	complete := func(c context.Context, id todo.TaskID) {
		must(tasks.Update(c, id, func(_ context.Context, t *todo.Task) error { return t.Complete() }))
	}

	// History that exists before any Reactor is deployed.
	old := newID()
	open(acme, old, "legacy task")

	// ── 0. Declaration ──────────────────────────────────────────────────────
	_, err := reactor.New(rt, reactor.Config{Name: "crm_sync", Handler: crmSync(store, tasks, crm)})
	step("0a. New without StartAt → ErrStartAtRequired", err)

	sync, err := reactor.New(rt, reactor.Config{
		Name:    "crm_sync",
		Handler: crmSync(store, tasks, crm),
		StartAt: reactor.FromNow,
	}, reactor.WithRetry(3, time.Millisecond), reactor.WithTimeout(5*time.Second))
	step("0b. New crm_sync, FromNow, 3 attempts", err)
	// In a service: consumer.Group{sync.Consumer(), projection..., ...}.Run(ctx)

	_ = sync.Drain(ctx)
	state("0c. First Drain: legacy history skipped (FromNow seeds the Cursor at the head)", sync, crm, store)

	// ── 1. Happy path + metadata stamping ───────────────────────────────────
	a := newID()
	open(acme, a, "write spec")
	complete(acme, a)
	_ = sync.Drain(ctx)
	state("1. Open+Complete → upsert, close, Archive command with actor/causation/correlation", sync, crm, store)

	// ── 2. Transient failure within the retry budget ────────────────────────
	b := newID()
	crm.flaky = 2
	open(acme, b, "flaky but fine")
	_ = sync.Drain(ctx)
	state("2. CRM fails twice, 3rd attempt succeeds → nothing parked", sync, crm, store)

	// ── 3. Attempts spent → park; later events of the stream queue behind ──
	c := newID()
	crm.broken["doomed v2"] = true
	open(acme, c, "doomed")
	rename(acme, c, "doomed v2")
	rename(acme, c, "doomed v3")
	d := newID()
	open(acme, d, "unrelated")
	_ = sync.Drain(ctx)
	state("3. c parked at 'doomed v2'; 'doomed v3' queued behind (not sent!); d flows", sync, crm, store)

	// ── 4. Same UUID in another tenant is not held ──────────────────────────
	open(globex, c, "globex same uuid")
	_ = sync.Drain(ctx)
	state("4. globex/c is a different stream (ADR-0002) → not held", sync, crm, store)

	// ── 5. Redrive while the CRM is still down ──────────────────────────────
	res, err := sync.Redrive(ctx, reactor.Stream{Tenant: "acme", ID: uuid.UUID(c)})
	crm.broken["doomed v2"] = false
	stepf("5. Redrive acme/c while CRM down → stops at head, nothing released", "replayed=%d remaining=%d err=%v", len(res.Replayed), res.Remaining, err)
	state("", sync, crm, store)

	// ── 6. Redrive after recovery → in order, hold released ─────────────────
	res, err = sync.Redrive(ctx, reactor.Stream{Tenant: "acme", ID: uuid.UUID(c)})
	stepf("6. Redrive acme/c after recovery", "replayed=%v err=%v", types(res.Replayed), err)
	state("", sync, crm, store)

	// ── 7. Permanent → park at once; Discard by an operator ────────────────
	e := newID()
	crm.reject["<bad>"] = true
	open(acme, e, "<bad>")
	rename(acme, e, "good title")
	_ = sync.Drain(ctx)
	state("7a. 422 is Permanent → parked after 1 attempt, rename queued", sync, crm, store)
	held, _ := sync.Parked(ctx, "acme")
	err = sync.Discard(ctx, reactor.Stream{Tenant: "acme", ID: uuid.UUID(e)}, held[0].EventID)
	stepf("7b. Discard the 422 head", "err=%v", err)
	res, err = sync.Redrive(ctx, reactor.Stream{Tenant: "acme", ID: uuid.UUID(e)})
	stepf("7c. Redrive replays what is left", "replayed=%v err=%v", types(res.Replayed), err)
	state("", sync, crm, store)

	// ── 8. Crash after the command committed, before the Cursor moved ──────
	f := newID()
	open(acme, f, "crashy")
	complete(acme, f)
	_ = sync.Drain(ctx)
	st, _ := sync.Status(ctx)
	sync.Consumer().Rewind(eventstore.Cursor{Position: st.Cursor.Position - 1})
	_ = sync.Drain(ctx)
	state("8. Completed delivered twice → CRM closed twice (at-least-once!), Archive no-op via ErrArchived", sync, crm, store)

	// ── 9. A second Reactor that replays history ────────────────────────────
	var audit []string
	auditR := consumer.NewRouter(store)
	consumer.On(auditR, func(ctx context.Context, env eventstore.Envelope, e todo.TaskArchived) error {
		audit = append(audit, fmt.Sprintf("%s %s", env.TenantID, e.CRMRef))
		return nil
	})
	auditor, err := reactor.New(rt, reactor.Config{Name: "archive_audit", Handler: auditR, StartAt: reactor.FromBeginning})
	must2(err)
	_ = auditor.Drain(ctx)
	stepf("9. archive_audit FromBeginning sees every TaskArchived ever written", "%v", audit)

	_, err = reactor.New(rt, reactor.Config{Name: "crm_sync", Handler: auditR, StartAt: reactor.FromNow})
	step("10. Same name twice → consumer.ErrDuplicate", err)
}

func step(name string, err error) { fmt.Printf("\n▶ %s\n  err: %v\n", name, err) }

func stepf(name, f string, args ...any) {
	fmt.Printf("\n▶ %s\n  %s\n", name, fmt.Sprintf(f, args...))
}

func state(name string, r *reactor.Reactor, crm *CRM, store *eventstore.InMemoryStore) {
	if name != "" {
		fmt.Printf("\n▶ %s\n", name)
	}
	st, _ := r.Status(context.Background())
	fmt.Printf("  status: cursor=%d pending=%d stopped=%v parked=%d streams/%d events\n",
		st.Cursor.Position, st.Pending, st.Stopped, st.ParkedStreams, st.ParkedEvents)
	held, _ := r.Parked(context.Background(), "")
	for _, p := range held {
		cause := p.Cause
		if cause == "" {
			cause = "(queued behind)"
		}
		fmt.Printf("  parked: %s/%s v%d %-20s attempts=%d %s\n", p.Tenant, p.StreamID.String()[28:], p.StreamPosition, p.EventType, p.Attempts, cause)
	}
	fmt.Printf("  crm:    %s\n", strings.Join(crm.calls, " | "))
	crm.calls = nil
	for _, e := range store.All() {
		if e.EventType == "Todo.Task.Archived" {
			fmt.Printf("  log:    %s/%s v%d %s actor=%s causation=%s correlation=%s\n",
				e.TenantID, e.StreamID.String()[28:], e.StreamPosition, e.EventType, e.Actor, e.CausationID, e.CorrelationID)
		}
	}
}

func types(ps []reactor.Parked) []string {
	var out []string
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s@v%d", p.EventType, p.StreamPosition))
	}
	return out
}

func must(_ eventstore.CommandResult, err error) {
	if err != nil {
		panic(err)
	}
}

func must2(err error) {
	if err != nil {
		panic(err)
	}
}
