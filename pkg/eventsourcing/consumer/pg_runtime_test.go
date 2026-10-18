package consumer_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

// startClaims runs Claims with pingInterval until the test ends.
func (e pgEnv) startClaims(t *testing.T, pingInterval time.Duration) *lease.Claims {
	t.Helper()
	claims := lease.NewClaims(e.pool, lease.Config{PingInterval: pingInterval})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = claims.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return claims
}

func (e pgEnv) writeAs(t *testing.T, tenant string, events ...aggregate.Event) eventstore.Cursor {
	t.Helper()
	ctx := ctxutil.WithTenantID(writerCtx(), tenant)
	res, err := e.notes.Create(ctx, noteID(uuid.Must(uuid.NewV7())), func(_ context.Context, n *note) error {
		for _, ev := range events {
			aggregate.Raise(n, ev)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	return res.Position
}

func TestNewPgRuntimeRejectsSchemaTooLongForChanges(t *testing.T) {
	// <schema>.events fits into 63 bytes, <schema>.changes does not.
	schema := strings.Repeat("s", 63-len(".events"))
	e := newPgEnv(t, pgEnvConfig{})
	store, err := eventstore.NewPgStore(eventstore.PgStoreConfig{Pool: e.pool, Codec: newCodec(), Schema: schema})
	if err != nil {
		t.Fatalf("NewPgStore: %v", err)
	}
	_, err = consumer.NewPgRuntime(consumer.PgRuntimeConfig{
		Pool: e.pool, Store: store, Claims: lease.NewClaims(e.pool, lease.Config{}),
	})
	if !errors.Is(err, notify.ErrInvalidChannel) {
		t.Errorf("NewPgRuntime = %v, want ErrInvalidChannel", err)
	}
}

// With SetLocalTenant every handler of a Cursor transaction sees the
// Tenant of its own event in app.tenant_id, also when one batch mixes
// Tenants.
func TestPgSetLocalTenantPerEvent(t *testing.T) {
	tests := []struct {
		name           string
		setLocalTenant bool
		want           []string
	}{
		{"on", true, []string{"t1", "t2", "t1"}},
		{"off", false, []string{"", "", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newPgEnv(t, pgEnvConfig{})
			e.writeAs(t, "t1", noteWritten{Text: "a"})
			e.writeAs(t, "t2", noteWritten{Text: "b"})
			e.writeAs(t, "t1", noteWritten{Text: "c"})
			rt, err := consumer.NewPgRuntime(consumer.PgRuntimeConfig{
				Pool: e.pool, Store: e.store, Claims: e.startClaims(t, time.Second),
				SetLocalTenant: tt.setLocalTenant,
			})
			if err != nil {
				t.Fatalf("NewPgRuntime: %v", err)
			}
			var seen []string
			c := newPgConsumer(t, rt, consumer.Config{
				Name: "tenant",
				Handler: consumer.HandlerFunc(func(ctx context.Context, _ eventstore.Envelope) error {
					var tenant string
					err := postgres.Conn(ctx, e.pool).
						QueryRow(ctx, `SELECT coalesce(current_setting('app.tenant_id', true), '')`).Scan(&tenant)
					seen = append(seen, tenant)
					return err
				}),
			})
			if err := c.Drain(context.Background()); err != nil {
				t.Fatalf("Drain: %v", err)
			}
			if !slices.Equal(seen, tt.want) {
				t.Errorf("app.tenant_id per event = %q, want %q", seen, tt.want)
			}
		})
	}
}

// A runner that lost its lease without noticing must not Stop the Consumer
// after the new lease holder moved the Cursor past the failing event.
func TestPgStopIsFencedByCursor(t *testing.T) {
	e := newPgEnv(t, pgEnvConfig{})
	e.write(t, noteWritten{Text: "a"})

	// The old holder pings rarely, so it does not notice the lost lease.
	oldClaims := e.startClaims(t, time.Hour)
	oldRT, err := consumer.NewPgRuntime(consumer.PgRuntimeConfig{Pool: e.pool, Store: e.store, Claims: oldClaims})
	if err != nil {
		t.Fatalf("NewPgRuntime: %v", err)
	}
	entered, fail := make(chan struct{}), make(chan struct{})
	old := newPgConsumer(t, oldRT, consumer.Config{
		Name: "fenced",
		Mode: consumer.OwnTransaction,
		Handler: consumer.HandlerFunc(func(context.Context, eventstore.Envelope) error {
			close(entered)
			<-fail
			return consumer.Permanent(errors.New("stale failure"))
		}),
	})
	done := make(chan error, 1)
	go func() { done <- old.Drain(context.Background()) }()
	<-entered

	// End the old holder's lock session, so its lease is free.
	_, err = e.pool.Exec(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND pid <> pg_backend_pid()
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`)
	if err != nil {
		t.Fatalf("terminate lock session: %v", err)
	}
	next := newPgConsumer(t, e.runtime(t), consumer.Config{Name: "fenced", Mode: consumer.OwnTransaction})
	waitDrained(t, next)

	close(fail)
	if err := <-done; errors.Is(err, consumer.ErrStopped) {
		t.Errorf("old Drain = %v, want a fenced failure", err)
	}
	if st := statusOf(t, next); st.Stopped != nil {
		t.Errorf("Status = %+v, want not Stopped by the old holder", st)
	}
}

// waitDrained drains c, retrying while the lease is still held elsewhere.
func waitDrained(t *testing.T, c *consumer.Consumer) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := c.Drain(context.Background())
		switch {
		case err == nil:
			return
		case !errors.Is(err, consumer.ErrRunning) || time.Now().After(deadline):
			t.Fatalf("Drain: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func statusOf(t *testing.T, c *consumer.Consumer) consumer.Status {
	t.Helper()
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return st
}
