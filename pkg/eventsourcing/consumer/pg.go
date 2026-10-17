package consumer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/internal/storehook"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

const (
	// connectWait bounds how long acquire waits for Claims to connect.
	connectWait  = 10 * time.Second
	connectRetry = 20 * time.Millisecond
	// statementTimeout bounds a Cursor update that outlives its ctx.
	statementTimeout = 5 * time.Second
)

// PgRuntimeConfig configures a PgRuntime.
type PgRuntimeConfig struct {
	// Pool runs the reads and Cursor transactions. Required.
	Pool *pgxpool.Pool
	// Store is the event log the Consumers read; its Schema qualifies the
	// consumer tables, the wake-up channel <schema>.events and the lease
	// namespace <schema>.consumer. Required.
	Store *eventstore.PgStore
	// Listener wakes Consumers on new events. Optional: without it the
	// Consumers only poll.
	Listener *notify.Listener
	// Claims grants each Consumer its lease. Required, and Claims.Run must
	// be running.
	Claims *lease.Claims
}

// PgRuntime runs Consumers on an eventstore.PgStore. Cursors live in the
// consumer_cursors table of docs/postgres/schema.sql. A Consumer reads only
// the events below pg_snapshot_xmin, so it never skips an event of a
// transaction that commits later with a smaller Cursor. A filtered Consumer
// reads one index range per event type.
//
// Each Consumer runs under a lease from Claims, also while Stopped. A
// Cursor transaction is a Postgres transaction in ctx: handler writes made
// through postgres.Conn commit or roll back together with the Cursor.
type PgRuntime struct {
	pool     *pgxpool.Pool
	codec    *eventstore.Codec
	uow      *postgres.PgUnitOfWork
	listener *notify.Listener
	claims   *lease.Claims
	channel  string
	ns       string
	queries  pgQueries

	mu    sync.Mutex
	ports map[string]*pgPort
}

// pgSource is the read side of eventstore.PgStore, reached through
// internal/storehook.
type pgSource interface {
	Codec() *eventstore.Codec
	Schema() string
}

// NewPgRuntime creates a PgRuntime. It fails with ErrInvalidConfig when
// Pool, Store or Claims is missing.
func NewPgRuntime(cfg PgRuntimeConfig) (*PgRuntime, error) {
	if cfg.Pool == nil || cfg.Claims == nil {
		return nil, fmt.Errorf("new pg runtime: Pool and Claims are required: %w", ErrInvalidConfig)
	}
	src, ok := storehook.PgStore(cfg.Store)
	if !ok {
		return nil, fmt.Errorf("new pg runtime: Store is required: %w", ErrInvalidConfig)
	}
	store := src.(pgSource)
	schema := store.Schema()
	return &PgRuntime{
		pool:     cfg.Pool,
		codec:    store.Codec(),
		uow:      postgres.NewUnitOfWork(cfg.Pool),
		listener: cfg.Listener,
		claims:   cfg.Claims,
		channel:  schema + ".events",
		ns:       schema + ".consumer",
		queries:  newPgQueries(schema),
		ports:    make(map[string]*pgPort),
	}, nil
}

// Resume clears the Stopped state of the named Consumer. It also works for
// a Consumer this Runtime does not run, as long as its Cursor exists.
func (rt *PgRuntime) Resume(ctx context.Context, name string) error {
	tag, err := rt.pool.Exec(ctx, rt.queries.resume, name)
	if err != nil {
		return fmt.Errorf("resume %q: %w", name, err)
	}
	rt.mu.Lock()
	p, attached := rt.ports[name]
	rt.mu.Unlock()
	if tag.RowsAffected() == 0 && !attached {
		return fmt.Errorf("resume %q: %w", name, ErrNotFound)
	}
	if attached {
		signal(p.resumedCh)
	}
	return nil
}

func (rt *PgRuntime) attach(in attachInput) (port, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if _, ok := rt.ports[in.name]; ok {
		return nil, fmt.Errorf("%q: %w", in.name, ErrDuplicate)
	}
	types := in.eventTypes
	if types == nil {
		types = []string{}
	}
	p := &pgPort{
		rt:        rt,
		name:      in.name,
		types:     types,
		mode:      in.mode,
		wake:      make(chan struct{}, 1),
		resumedCh: make(chan struct{}, 1),
	}
	rt.ports[in.name] = p
	return p, nil
}

// signal sends on ch without blocking; ch has a buffer of one.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

type pgPort struct {
	rt    *PgRuntime
	name  string
	types []string
	mode  Mode
	// held is set while this port holds the lease.
	held atomic.Bool
	// wake receives when a NOTIFY (or Gap) arrived on the subscription of
	// the current lease.
	wake      chan struct{}
	resumedCh chan struct{}
}

func (p *pgPort) codec() *eventstore.Codec { return p.rt.codec }

// acquire takes the lease, claims the Cursor row and subscribes to new
// events until release.
func (p *pgPort) acquire(ctx context.Context) (context.Context, func(), error) {
	leaseCtx, l, err := p.tryAcquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := p.claimFilter(leaseCtx); err != nil {
		l.Release()
		return nil, nil, fmt.Errorf("claim cursor: %w", err)
	}
	stopWake, err := p.subscribe(leaseCtx)
	if err != nil {
		l.Release()
		return nil, nil, err
	}
	p.held.Store(true)
	var once sync.Once
	return leaseCtx, func() {
		once.Do(func() {
			p.held.Store(false)
			stopWake()
			l.Release()
		})
	}, nil
}

// tryAcquire takes the Consumer's lease, waiting up to connectWait for
// Claims to connect. A lease held by this process or another session is
// ErrRunning.
func (p *pgPort) tryAcquire(ctx context.Context) (context.Context, *lease.Lease, error) {
	key := lease.Key{Namespace: p.rt.ns, Name: p.name}
	deadline := time.Now().Add(connectWait)
	for {
		leaseCtx, l, err := p.rt.claims.TryAcquire(ctx, key)
		switch {
		case err == nil:
			return leaseCtx, l, nil
		case errors.Is(err, lease.ErrHeld):
			return nil, nil, fmt.Errorf("%w: %w", ErrRunning, err)
		case !errors.Is(err, lease.ErrNotConnected) || time.Now().After(deadline):
			return nil, nil, fmt.Errorf("acquire lease: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, nil, context.Cause(ctx)
		case <-time.After(connectRetry):
		}
	}
}

// subscribe forwards every notification on the events channel to p.wake
// until the returned stop is called or ctx ends. Without a Listener it does
// nothing.
func (p *pgPort) subscribe(ctx context.Context) (stop func(), err error) {
	if p.rt.listener == nil {
		return func() {}, nil
	}
	sub, err := p.rt.listener.Subscribe(p.rt.channel)
	if err != nil {
		return nil, fmt.Errorf("subscribe %s: %w", p.rt.channel, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-sub.C():
				if !ok {
					return
				}
				signal(p.wake)
			}
		}
	}()
	return func() {
		cancel()
		<-done
		sub.Close()
	}, nil
}

// claimFilter creates the Cursor row with the configured filter, or
// records a changed filter on it. It stops the Consumer instead when the
// change would leave a gap. A Stopped Cursor is checked again once resumed.
func (p *pgPort) claimFilter(ctx context.Context) error {
	return pgx.BeginFunc(ctx, p.rt.pool, func(tx pgx.Tx) error {
		var (
			cursor  eventstore.Cursor
			stored  []string
			stopped bool
		)
		err := tx.QueryRow(ctx, p.rt.queries.lockCursor, p.name).
			Scan(&cursor.TransactionID, &cursor.Position, &stored, &stopped)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_, err = tx.Exec(ctx, p.rt.queries.claim, p.name, p.types)
			return err
		case err != nil:
			return err
		case stopped || slices.Equal(stored, p.types):
			return nil
		}
		change := diffFilter(stored, p.types)
		if change.needsHistoryCheck(p.mode) {
			gap, err := p.hasHistory(ctx, tx, change, cursor)
			if err != nil {
				return fmt.Errorf("check filter change: %w", err)
			}
			if gap {
				_, err := tx.Exec(ctx, p.rt.queries.stop, p.name, time.Now(), change.cause())
				return err
			}
		}
		_, err = tx.Exec(ctx, p.rt.queries.setFilter, p.name, p.types)
		return err
	})
}

// hasHistory reports whether an event of a type added by f lies at or below
// upTo.
func (p *pgPort) hasHistory(ctx context.Context, tx pgx.Tx, f filterChange, upTo eventstore.Cursor) (bool, error) {
	query, types := p.rt.queries.historyFiltered, f.added
	if f.widened {
		query, types = p.rt.queries.historyOutside, f.stored
	}
	var found bool
	err := tx.QueryRow(ctx, query, types, xid(upTo), upTo.Position).Scan(&found)
	return found, err
}

// load reads the Cursor. While this port holds the lease, a filter that
// differs from the stored one is claimed first, so a Resume can't skip the
// filter check.
func (p *pgPort) load(ctx context.Context) (cursorState, error) {
	state, stored, err := p.loadRow(ctx)
	if err != nil || !p.held.Load() || state.stopped != nil || slices.Equal(stored, p.types) {
		return state, err
	}
	if err := p.claimFilter(ctx); err != nil {
		return cursorState{}, fmt.Errorf("claim cursor: %w", err)
	}
	state, _, err = p.loadRow(ctx)
	return state, err
}

// loadRow reads the Cursor and its stored filter. A missing row reads as a
// zero Cursor whose filter equals the configured one.
func (p *pgPort) loadRow(ctx context.Context) (cursorState, []string, error) {
	var (
		state     cursorState
		stored    []string
		stopped   bool
		stoppedAt *time.Time
		cause     *string
	)
	err := p.rt.pool.QueryRow(ctx, p.rt.queries.load, p.name).
		Scan(&state.cursor.TransactionID, &state.cursor.Position, &stored, &stopped, &stoppedAt, &cause)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return cursorState{}, p.types, nil
	case err != nil:
		return cursorState{}, nil, err
	}
	if stopped {
		state.stopped = &Stop{}
		if stoppedAt != nil {
			state.stopped.At = stoppedAt.UTC()
		}
		if cause != nil {
			state.stopped.Cause = *cause
		}
	}
	return state, stored, nil
}

func (p *pgPort) read(ctx context.Context, in readInput) ([]record, error) {
	query, args := p.rt.queries.read, []any{xid(in.after), in.after.Position, in.limit}
	if len(p.types) > 0 {
		query, args = p.rt.queries.readFiltered, append([]any{p.types}, args...)
	}
	rows, err := p.rt.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRecord)
}

func scanRecord(row pgx.CollectableRow) (record, error) {
	var (
		r         record
		causation *uuid.UUID
	)
	err := row.Scan(&r.env.Cursor.TransactionID, &r.env.Cursor.Position, &r.env.EventID, &r.env.TenantID,
		&r.env.StreamType, &r.env.StreamID, &r.env.StreamPosition, &r.payload.EventType,
		&r.payload.SchemaVersion, &r.payload.Data, &r.env.Actor, &r.env.CorrelationID, &causation,
		&r.env.RecordedAt)
	if err != nil {
		return record{}, err
	}
	r.env.EventType = r.payload.EventType
	r.env.RecordedAt = r.env.RecordedAt.UTC()
	if causation != nil {
		r.env.CausationID = *causation
	}
	return r, nil
}

func (p *pgPort) head(ctx context.Context) (eventstore.Cursor, error) {
	query, args := p.rt.queries.head, []any(nil)
	if len(p.types) > 0 {
		query, args = p.rt.queries.headFiltered, []any{p.types}
	}
	var c eventstore.Cursor
	err := p.rt.pool.QueryRow(ctx, query, args...).Scan(&c.TransactionID, &c.Position)
	if errors.Is(err, pgx.ErrNoRows) {
		return eventstore.Cursor{}, nil
	}
	return c, err
}

func (p *pgPort) status(ctx context.Context) (Status, error) {
	state, err := p.load(ctx)
	if err != nil {
		return Status{}, err
	}
	s := Status{Cursor: state.cursor, Stopped: state.stopped}

	query, args := p.rt.queries.pending, []any{xid(state.cursor), state.cursor.Position, PendingCap + 1}
	if len(p.types) > 0 {
		query, args = p.rt.queries.pendingFiltered, append([]any{p.types}, args...)
	}
	if err := p.rt.pool.QueryRow(ctx, query, args...).Scan(&s.Pending); err != nil {
		return Status{}, fmt.Errorf("count pending: %w", err)
	}
	if s.Pending == 0 {
		return s, nil
	}
	oldest, err := p.read(ctx, readInput{after: state.cursor, limit: 1})
	if err != nil {
		return Status{}, fmt.Errorf("read oldest pending: %w", err)
	}
	if len(oldest) > 0 {
		s.Age = max(time.Since(oldest[0].env.RecordedAt), 0)
	}
	return s, nil
}

func (p *pgPort) transact(ctx context.Context, fn func(ctx context.Context) error) error {
	return p.rt.uow.Execute(ctx, fn)
}

// advance outside a Cursor transaction records an event whose handler
// already took effect (OwnTransaction), so it completes even when ctx ends.
func (p *pgPort) advance(ctx context.Context, m move) error {
	if postgres.TxFromContext(ctx) == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), statementTimeout)
		defer cancel()
	}
	tag, err := postgres.Conn(ctx, p.rt.pool).Exec(ctx, p.rt.queries.advance,
		p.name, xid(m.from), m.from.Position, xid(m.to), m.to.Position)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cursor %q not at %v: %w", p.name, m.from, errFenced)
	}
	return nil
}

func (p *pgPort) stop(ctx context.Context, s Stop) error {
	_, err := p.rt.pool.Exec(ctx, p.rt.queries.stop, p.name, s.At, s.Cause)
	return err
}

func (p *pgPort) changed() <-chan struct{} { return p.wake }

func (p *pgPort) resumed() <-chan struct{} { return p.resumedCh }

// xid renders the transaction ID of c for a ::text::xid8 parameter.
func xid(c eventstore.Cursor) string {
	return strconv.FormatUint(c.TransactionID, 10)
}

// pgQueries are the runtime's statements for one schema. Filtered reads
// take one ordered index range per event type through unnest + LATERAL;
// unfiltered ones read the log-order index. Both only see events below
// pg_snapshot_xmin.
type pgQueries struct {
	claim, lockCursor, setFilter, load, advance, stop, resume string

	// historyFiltered and historyOutside check for events of the given
	// types, or of any type outside them, at or below a Cursor.
	historyFiltered, historyOutside string

	read, readFiltered       string
	head, headFiltered       string
	pending, pendingFiltered string
}

const visible = `e.transaction_id < pg_snapshot_xmin(pg_current_snapshot())`

func newPgQueries(schema string) pgQueries {
	events := pgx.Identifier{schema, "events"}.Sanitize()
	cursors := pgx.Identifier{schema, "consumer_cursors"}.Sanitize()
	columns := `e.transaction_id::text::numeric, e.global_position, e.event_id, e.tenant_id,
		e.stream_type, e.stream_id, e.stream_position, e.event_type, e.schema_version, e.event_data,
		e.actor, e.correlation_id, e.causation_id, e.recorded_at`
	order := `ORDER BY e.transaction_id, e.global_position`
	orderDesc := `ORDER BY e.transaction_id DESC, e.global_position DESC`
	// after binds the Cursor to $n (transaction ID) and $n+1 (position).
	after := func(n int) string {
		return fmt.Sprintf(`(e.transaction_id, e.global_position) > ($%d::text::xid8, $%d::bigint)`, n, n+1)
	}
	// typeAfter and typeOrder follow the columns of the event type index,
	// so a generic plan reads one range of it per type instead of filtering
	// the log-order index.
	typeAfter := func(n int) string {
		return fmt.Sprintf(`e.event_type = t.event_type
			AND (e.event_type, e.transaction_id, e.global_position) > (t.event_type, $%d::text::xid8, $%d::bigint)`, n, n+1)
	}
	typeOrder := `ORDER BY e.event_type, e.transaction_id, e.global_position`
	typeOrderDesc := `ORDER BY e.event_type DESC, e.transaction_id DESC, e.global_position DESC`
	perType := func(inner string) string {
		return `FROM unnest($1::text[]) AS t(event_type) CROSS JOIN LATERAL (` + inner + `) e`
	}

	return pgQueries{
		claim: `INSERT INTO ` + cursors + ` (name, event_types) VALUES ($1, $2)
			ON CONFLICT (name) DO NOTHING`,
		lockCursor: `SELECT transaction_id::text::numeric, global_position, event_types, stopped
			FROM ` + cursors + ` WHERE name = $1 FOR UPDATE`,
		setFilter: `UPDATE ` + cursors + ` SET event_types = $2 WHERE name = $1`,
		load: `SELECT transaction_id::text::numeric, global_position, event_types, stopped, stopped_at, stopped_error
			FROM ` + cursors + ` WHERE name = $1`,
		advance: `UPDATE ` + cursors + ` SET transaction_id = $4::text::xid8, global_position = $5
			WHERE name = $1 AND transaction_id = $2::text::xid8 AND global_position = $3`,
		stop: `UPDATE ` + cursors + ` SET stopped = true, stopped_at = $2, stopped_error = $3
			WHERE name = $1`,
		resume: `UPDATE ` + cursors + ` SET stopped = false, stopped_at = NULL, stopped_error = NULL
			WHERE name = $1`,

		historyFiltered: `SELECT EXISTS (SELECT 1 ` + perType(`SELECT 1 FROM `+events+` e
				WHERE e.event_type = t.event_type
				AND (e.event_type, e.transaction_id, e.global_position) <= (t.event_type, $2::text::xid8, $3::bigint)
				`+typeOrder+` LIMIT 1`) + `)`,
		historyOutside: `SELECT EXISTS (SELECT 1 FROM ` + events + ` e
			WHERE e.event_type <> ALL($1::text[])
			AND (e.transaction_id, e.global_position) <= ($2::text::xid8, $3::bigint))`,

		read: `SELECT ` + columns + ` FROM ` + events + ` e
			WHERE ` + after(1) + ` AND ` + visible + `
			` + order + ` LIMIT $3`,
		readFiltered: `SELECT ` + columns + ` ` + perType(`SELECT * FROM `+events+` e
				WHERE `+typeAfter(2)+` AND `+visible+`
				`+typeOrder+` LIMIT $4`) + `
			` + order + ` LIMIT $4`,

		head: `SELECT e.transaction_id::text::numeric, e.global_position FROM ` + events + ` e
			WHERE ` + visible + ` ` + orderDesc + ` LIMIT 1`,
		headFiltered: `SELECT e.transaction_id::text::numeric, e.global_position ` +
			perType(`SELECT e.transaction_id, e.global_position FROM `+events+` e
				WHERE e.event_type = t.event_type AND `+visible+` `+typeOrderDesc+` LIMIT 1`) + `
			` + orderDesc + ` LIMIT 1`,

		pending: `SELECT count(*)::int FROM (SELECT 1 FROM ` + events + ` e
			WHERE ` + after(1) + ` AND ` + visible + ` ` + order + ` LIMIT $3) pending`,
		pendingFiltered: `SELECT least(COALESCE(sum(e.n), 0), $4)::int ` +
			perType(`SELECT count(*) AS n FROM (SELECT 1 FROM `+events+` e
				WHERE `+typeAfter(2)+` AND `+visible+`
				`+typeOrder+` LIMIT $4) pending`),
	}
}

var _ Runtime = (*PgRuntime)(nil)
