package eventstore

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/postgres"
)

// InMemoryStore is an event log in memory, for prototypes and tests. Like
// the Postgres store it encodes every event with its Codec and upcasts on
// load. Appends become visible to others when the InMemoryUnitOfWork
// commits and are dropped when it rolls back.
type InMemoryStore struct {
	codec *Codec
	clock func() time.Time
	log   *memLog
}

// NewInMemoryStore creates an empty InMemoryStore that encodes with codec.
// A nil codec is a programming error, so it panics instead of returning
// ErrInvalidConfig.
func NewInMemoryStore(codec *Codec, opts ...StoreOption) *InMemoryStore {
	if codec == nil {
		panic("eventstore: NewInMemoryStore: nil codec")
	}
	o := newStoreOptions(opts)
	return &InMemoryStore{
		codec: codec,
		clock: o.clock,
		log: &memLog{
			streams:  make(map[streamKey][]memRecord),
			byType:   make(map[string][]memRecord),
			pending:  make(map[any]*memPending),
			commands: make(map[commandKey]memCommand),
			changed:  make(chan struct{}),
		},
	}
}

type commandKey struct {
	tenant string
	id     uuid.UUID
}

// memCommand is a processed command as stored.
type memCommand struct {
	cmd        processedCommand
	recordedAt time.Time
}

type streamKey struct {
	tenant string
	stream uuid.UUID
}

// memRecord is an event as stored: its Envelope without the decoded Event,
// plus its payload and the traceparent of the send span that appended it.
type memRecord struct {
	env          Envelope
	payload      Payload
	traceContext string
}

type memLog struct {
	mu      sync.Mutex
	streams map[streamKey][]memRecord
	// all holds every committed record ordered by Cursor, byType the same
	// per event type.
	all     []memRecord
	byType  map[string][]memRecord
	lastXID uint64
	lastPos int64
	// pending holds the writes of open transactions by commandTx.pendingKey.
	pending map[any]*memPending
	// commands holds the committed processed commands.
	commands map[commandKey]memCommand
	// changed is closed and replaced whenever a commit or rollback may have
	// made more events visible.
	changed chan struct{}
}

type memPending struct {
	xid      uint64
	records  []memRecord
	commands map[commandKey]memCommand
}

func (s *InMemoryStore) load(ctx context.Context, in loadInput) ([]Envelope, error) {
	records := s.log.stream(ctx, streamKey{tenant: in.tenant, stream: in.stream})
	envs := make([]Envelope, 0, len(records))
	for _, r := range records {
		if in.upTo > 0 && r.env.StreamPosition > in.upTo {
			break
		}
		e, err := s.codec.Decode(r.payload)
		if err != nil {
			return nil, fmt.Errorf("load %s position %d: %w", in.stream, r.env.StreamPosition, err)
		}
		env := r.env
		env.Event = e
		envs = append(envs, env)
	}
	return envs, nil
}

// stream returns the committed records of k, plus the pending ones when ctx
// runs in the Command transaction that holds k.
func (l *memLog) stream(ctx context.Context, k streamKey) []memRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	records := slices.Clone(l.streams[k])
	if tx, ok := commandTxFrom(ctx); ok && tx.tenant == k.tenant && tx.stream == k.stream {
		if p := l.pending[tx.pendingKey()]; p != nil {
			records = append(records, p.records...)
		}
	}
	return records
}

func (s *InMemoryStore) append(ctx context.Context, in appendInput) (Cursor, error) {
	meta, err := resolveMetadata(ctx, in.tx)
	if err != nil {
		return Cursor{}, err
	}
	payloads := make([]Payload, len(in.events))
	for i, e := range in.events {
		if payloads[i], err = s.codec.Encode(e); err != nil {
			return Cursor{}, err
		}
	}
	eventIDs := make([]uuid.UUID, len(in.events))
	for i := range eventIDs {
		if eventIDs[i], err = uuid.NewV7(); err != nil {
			return Cursor{}, fmt.Errorf("generate event id: %w", err)
		}
	}

	template := Envelope{
		TenantID:      in.tx.tenant,
		StreamType:    in.streamType,
		StreamID:      in.tx.stream,
		Actor:         meta.actor,
		CorrelationID: meta.correlation,
		CausationID:   meta.causation,
		RecordedAt:    s.clock(),
	}
	last, opened, err := s.log.stage(stageInput{
		tx:           in.tx,
		expected:     in.expected,
		template:     template,
		payloads:     payloads,
		eventIDs:     eventIDs,
		traceContext: in.traceContext,
	})
	if err != nil {
		return Cursor{}, err
	}
	if opened {
		s.settle(ctx, in.tx)
	}
	return last, nil
}

// settle commits the pending writes of tx with its in-memory transaction,
// or discards them on rollback.
func (s *InMemoryStore) settle(ctx context.Context, tx *commandTx) {
	postgres.OnRollback(ctx, func() { s.log.discard(tx) })
	postgres.Stage(ctx, func() { s.log.commit(tx) })
}

func (s *InMemoryStore) findCommand(_ context.Context, tx *commandTx, id uuid.UUID) (processedCommand, bool, error) {
	cmd, ok := s.log.findCommand(tx, commandKey{tenant: tx.tenant, id: id})
	return cmd, ok, nil
}

func (s *InMemoryStore) recordCommand(ctx context.Context, tx *commandTx, cmd processedCommand) error {
	opened, err := s.log.recordCommand(tx, memCommand{cmd: cmd, recordedAt: s.clock()})
	if err != nil {
		return err
	}
	if opened {
		s.settle(ctx, tx)
	}
	return nil
}

// CleanupCommands deletes the committed Command IDs recorded before the
// cutoff, for all Tenants. A repeat of a deleted Command ID runs as a new command.
func (s *InMemoryStore) CleanupCommands(_ context.Context, before time.Time) (int64, error) {
	return s.log.cleanupCommands(before), nil
}

func (s *InMemoryStore) now() time.Time { return s.clock() }

// cleanupCommands deletes the committed commands recorded before the cutoff
// and returns how many it deleted.
func (l *memLog) cleanupCommands(before time.Time) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var deleted int64
	for k, c := range l.commands {
		if c.recordedAt.Before(before) {
			delete(l.commands, k)
			deleted++
		}
	}
	return deleted
}

// findCommand returns the committed command under k, or the one pending in
// tx.
func (l *memLog) findCommand(tx *commandTx, k commandKey) (processedCommand, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.commands[k]; ok {
		return c.cmd, true
	}
	if p := l.pending[tx.pendingKey()]; p != nil {
		if c, ok := p.commands[k]; ok {
			return c.cmd, true
		}
	}
	return processedCommand{}, false
}

// recordCommand adds c to the pending writes of tx. Like the primary key
// in Postgres, it fails when any transaction, tx included, holds the same
// key. opened reports whether this is the transaction's first write.
func (l *memLog) recordCommand(tx *commandTx, c memCommand) (opened bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := commandKey{tenant: tx.tenant, id: c.cmd.id}
	if l.commandTaken(k) {
		return false, fmt.Errorf("command %s recorded concurrently: %w", c.cmd.id, ErrConcurrencyConflict)
	}
	p, opened := l.openPending(tx)
	p.commands[k] = c
	return opened, nil
}

// commandTaken reports whether k is committed or pending in any open
// transaction. The caller holds l.mu.
func (l *memLog) commandTaken(k commandKey) bool {
	if _, ok := l.commands[k]; ok {
		return true
	}
	for _, p := range l.pending {
		if _, ok := p.commands[k]; ok {
			return true
		}
	}
	return false
}

// openPending returns the pending writes of tx, creating them on its first
// write. The caller holds l.mu.
func (l *memLog) openPending(tx *commandTx) (p *memPending, opened bool) {
	if p := l.pending[tx.pendingKey()]; p != nil {
		return p, false
	}
	l.lastXID++
	p = &memPending{xid: l.lastXID, commands: make(map[commandKey]memCommand)}
	l.pending[tx.pendingKey()] = p
	return p, true
}

type stageInput struct {
	tx           *commandTx
	expected     int
	template     Envelope
	payloads     []Payload
	eventIDs     []uuid.UUID
	traceContext string
}

// stage adds records to the pending writes of in.tx. opened reports
// whether this is the transaction's first write.
func (l *memLog) stage(in stageInput) (last Cursor, opened bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	k := streamKey{tenant: in.tx.tenant, stream: in.tx.stream}
	p := l.pending[in.tx.pendingKey()]
	version := len(l.streams[k])
	if p != nil {
		version += len(p.records)
	}
	if version != in.expected {
		return Cursor{}, false, fmt.Errorf("stream %s at version %d, expected %d: %w", in.tx.stream, version, in.expected, ErrConcurrencyConflict)
	}
	p, opened = l.openPending(in.tx)
	for i, payload := range in.payloads {
		l.lastPos++
		env := in.template
		env.Cursor = Cursor{TransactionID: p.xid, Position: l.lastPos}
		env.EventID = in.eventIDs[i]
		env.StreamPosition = in.expected + i + 1
		env.EventType = payload.EventType
		p.records = append(p.records, memRecord{env: env, payload: payload, traceContext: in.traceContext})
		last = env.Cursor
	}
	return last, opened, nil
}

func (l *memLog) commit(tx *commandTx) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.pending[tx.pendingKey()]
	delete(l.pending, tx.pendingKey())
	if p == nil {
		return
	}
	maps.Copy(l.commands, p.commands)
	k := streamKey{tenant: tx.tenant, stream: tx.stream}
	l.streams[k] = append(l.streams[k], p.records...)
	for _, r := range p.records {
		l.all = insertByCursor(l.all, r)
		l.byType[r.env.EventType] = insertByCursor(l.byType[r.env.EventType], r)
	}
	l.broadcast()
}

func insertByCursor(records []memRecord, r memRecord) []memRecord {
	i, _ := slices.BinarySearchFunc(records, r.env.Cursor, compareRecord)
	return slices.Insert(records, i, r)
}

func compareRecord(r memRecord, c Cursor) int { return r.env.Cursor.Compare(c) }

func (l *memLog) discard(tx *commandTx) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pending, tx.pendingKey())
	l.broadcast()
}

// broadcast wakes every waiter on changed. The caller holds l.mu.
func (l *memLog) broadcast() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// InMemoryUnitOfWork runs Command transactions in memory. It serializes
// them per (Tenant, Stream) and runs each inside a
// postgres.InMemoryUnitOfWork, so InMemoryStore appends and in-memory guard,
// numbering and Stage writes commit or roll back together.
type InMemoryUnitOfWork struct {
	tx    *postgres.InMemoryUnitOfWork
	mu    sync.Mutex
	locks map[streamKey]*streamLock
	// held is the Stream each open in-memory transaction holds, by
	// postgres.InMemoryTransaction.
	held map[any]streamKey
}

type streamLock struct {
	sem  chan struct{}
	refs int
}

// NewInMemoryUnitOfWork creates an InMemoryUnitOfWork.
func NewInMemoryUnitOfWork() *InMemoryUnitOfWork {
	return &InMemoryUnitOfWork{
		tx:    postgres.NewInMemoryUnitOfWork(),
		locks: make(map[streamKey]*streamLock),
		held:  make(map[any]streamKey),
	}
}

// Execute opens a Command transaction for stream, waiting until no other
// one holds the same (Tenant, Stream) or ctx ends. An open
// postgres.InMemoryUnitOfWork transaction in ctx is joined: the stream lock
// is then held until that transaction ends, and joining one that already
// holds another Stream fails with ErrStreamMismatch.
func (u *InMemoryUnitOfWork) Execute(ctx context.Context, stream uuid.UUID, fn func(ctx context.Context) error) error {
	ctx, tx, err := openCommandTx(ctx, stream)
	if err != nil {
		return err
	}
	return u.tx.Execute(ctx, func(ctx context.Context) error {
		tx.txn, _ = postgres.InMemoryTransaction(ctx)
		if err := u.hold(ctx, streamKey{tenant: tx.tenant, stream: stream}); err != nil {
			return err
		}
		return fn(ctx)
	})
}

// hold locks k for the in-memory transaction in ctx until it ends. A
// transaction that already holds k keeps it.
func (u *InMemoryUnitOfWork) hold(ctx context.Context, k streamKey) error {
	txn, _ := postgres.InMemoryTransaction(ctx)
	u.mu.Lock()
	held, ok := u.held[txn]
	u.mu.Unlock()
	switch {
	case ok && held != k:
		return fmt.Errorf("transaction holds %s/%s, got %s/%s: %w", held.tenant, held.stream, k.tenant, k.stream, ErrStreamMismatch)
	case ok:
		return nil
	}
	unlock, err := u.lock(ctx, k)
	if err != nil {
		return err
	}
	u.mu.Lock()
	u.held[txn] = k
	u.mu.Unlock()
	postgres.OnComplete(ctx, func() {
		u.mu.Lock()
		delete(u.held, txn)
		u.mu.Unlock()
		unlock()
	})
	return nil
}

// Within joins the open Command transaction for stream, or opens one.
func (u *InMemoryUnitOfWork) Within(ctx context.Context, stream uuid.UUID, fn func(ctx context.Context) error) error {
	return executor(u.Execute).within(ctx, stream, fn)
}

func (u *InMemoryUnitOfWork) lock(ctx context.Context, k streamKey) (unlock func(), err error) {
	u.mu.Lock()
	l := u.locks[k]
	if l == nil {
		l = &streamLock{sem: make(chan struct{}, 1)}
		u.locks[k] = l
	}
	l.refs++
	u.mu.Unlock()

	release := func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		l.refs--
		if l.refs == 0 {
			delete(u.locks, k)
		}
	}
	select {
	case l.sem <- struct{}{}:
		return func() {
			<-l.sem
			release()
		}, nil
	case <-ctx.Done():
		release()
		return nil, fmt.Errorf("lock stream %s: %w", k.stream, context.Cause(ctx))
	}
}

var (
	_ Store      = (*InMemoryStore)(nil)
	_ UnitOfWork = (*InMemoryUnitOfWork)(nil)
)
