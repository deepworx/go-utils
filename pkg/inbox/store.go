// Package inbox is a durable, deduplicated, leased work queue in Postgres
// for incoming external messages such as webhooks. It is independent of the
// event log.
//
// A Store inserts Messages inside the caller's transaction, deduplicated by
// (Source, Tenant, Key). Authenticate a message before inserting it: a
// forged message with a real Key would drop the real delivery (ADR-0015). A Worker per Source claims due messages, runs its Handler
// and settles each message as done, ignored, Parked or due for a retry.
// Every pod runs every Source's Worker; rows are claimed with
// FOR UPDATE SKIP LOCKED and fenced by a claim generation, so a Worker whose
// claim expired can never settle a message claimed again elsewhere. There is
// no ordering guarantee.
//
// Operators List messages, Redrive Parked ones and Discard the ones that
// should not be handled. These calls span every Tenant and do no
// authorization (ADR-0014): expose them only to Operators. CleanupJob
// deletes settled messages after a retention period; Parked messages stay
// until an Operator resolves them.
package inbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/propagation"

	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

const defaultSchema = "public"

const maxKeyLen = 256

// Message is an incoming external message.
type Message struct {
	// Source names the origin of the message, such as one webhook provider.
	// Required.
	Source string
	// Key identifies the message within its Source and Tenant. Required, at
	// most 256 bytes.
	Key string
	// Type classifies the message for the Handler. Required.
	Type string
	// Tenant is the message's Tenant, or empty for none.
	Tenant string
	// Payload is the raw message body.
	Payload []byte
}

func (m Message) validate() error {
	switch {
	case m.Source == "":
		return fmt.Errorf("%w: empty source", ErrInvalidMessage)
	case m.Key == "":
		return fmt.Errorf("%w: empty key", ErrInvalidMessage)
	case len(m.Key) > maxKeyLen:
		return fmt.Errorf("%w: key longer than %d bytes", ErrInvalidMessage, maxKeyLen)
	case m.Type == "":
		return fmt.Errorf("%w: empty type", ErrInvalidMessage)
	}
	return nil
}

// StoreConfig configures a Store.
type StoreConfig struct {
	// Schema qualifies the inbox_messages table and derives the notify
	// channel <schema>.inbox. Defaults to public.
	Schema string
}

// StoreOption configures a Store beyond StoreConfig.
type StoreOption func(*Store)

// WithClock sets the time source for received, due, claim and settle
// times. It defaults to time.Now().UTC(). Workers use their Store's clock.
func WithClock(now func() time.Time) StoreOption {
	return func(s *Store) { s.now = now }
}

// Store reads and writes the inbox_messages table.
type Store struct {
	pool    *pgxpool.Pool
	table   string
	channel string
	now     func() time.Time
}

// NewStore returns a Store on pool. It fails with notify.ErrInvalidChannel
// when the schema makes the derived channel name invalid, and with
// ErrInvalidConfig without a pool.
func NewStore(pool *pgxpool.Pool, cfg StoreConfig, opts ...StoreOption) (*Store, error) {
	if cfg.Schema == "" {
		cfg.Schema = defaultSchema
	}
	channel := cfg.Schema + ".inbox"
	if err := notify.CheckChannel(channel); err != nil {
		return nil, fmt.Errorf("inbox: schema %q: %w", cfg.Schema, err)
	}
	if pool == nil {
		return nil, fmt.Errorf("%w: nil pool", ErrInvalidConfig)
	}
	s := &Store{
		pool:    pool,
		table:   pgx.Identifier{cfg.Schema, "inbox_messages"}.Sanitize(),
		channel: channel,
		now:     func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Insert stores m as a pending message, inside the transaction carried by
// ctx when there is one. It reports false when a message with the same
// Source, Tenant and Key already exists, whatever its status; an empty
// Tenant only matches an empty Tenant. Authenticate m before calling Insert
// and reject it without inserting when that fails (ADR-0015). It fails with
// ErrInvalidMessage when Source, Key or Type is empty. The current span's
// trace context is stored so the handling span can link to it.
func (s *Store) Insert(ctx context.Context, m Message) (inserted bool, err error) {
	if err := m.validate(); err != nil {
		return false, err
	}
	payload := m.Payload
	if payload == nil {
		payload = []byte{}
	}
	now := s.now()
	var id int64
	err = postgres.Conn(ctx, s.pool).QueryRow(ctx, `
		INSERT INTO `+s.table+` (source, key, type, tenant_id, payload, next_attempt_at, received_at, trace_context)
		VALUES ($1, $2, $3, $4, $5, $6, $6, NULLIF($7, ''))
		ON CONFLICT (source, tenant_id, key) DO NOTHING
		RETURNING id`,
		m.Source, m.Key, m.Type, m.Tenant, payload, now, traceParent(ctx),
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inbox: insert %s/%s (tenant %q): %w", m.Source, m.Key, m.Tenant, err)
	}
	return true, nil
}

func traceParent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}
