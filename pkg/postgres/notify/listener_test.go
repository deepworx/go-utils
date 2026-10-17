package notify_test

import (
	"context"
	"errors"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

func TestCheckChannel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		channel string
		wantErr error
	}{
		{name: "plain", channel: "public.events"},
		{name: "needs quotes", channel: `My "Channel"`},
		{name: "63 bytes", channel: strings.Repeat("a", 63)},
		{name: "64 bytes", channel: strings.Repeat("a", 64), wantErr: notify.ErrInvalidChannel},
		{name: "multibyte over limit", channel: strings.Repeat("ä", 32), wantErr: notify.ErrInvalidChannel},
		{name: "empty", channel: "", wantErr: notify.ErrInvalidChannel},
		{name: "nul byte", channel: "a\x00b", wantErr: notify.ErrInvalidChannel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := notify.CheckChannel(tt.channel); !errors.Is(err, tt.wantErr) {
				t.Errorf("CheckChannel(%q) = %v, want %v", tt.channel, err, tt.wantErr)
			}
		})
	}
}

const waitTimeout = 10 * time.Second

func TestNewListenerRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	if _, err := notify.NewListener(nil, notify.Config{}); !errors.Is(err, notify.ErrNilPool) {
		t.Errorf("NewListener(nil) = %v, want ErrNilPool", err)
	}
}

func TestSubscribeRejectsInvalidChannel(t *testing.T) {
	t.Parallel()
	l, err := notify.NewListener(&pgxpool.Pool{}, notify.Config{})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	if _, err := l.Subscribe(strings.Repeat("x", 64)); !errors.Is(err, notify.ErrInvalidChannel) {
		t.Errorf("Subscribe(64 bytes) = %v, want ErrInvalidChannel", err)
	}
}

// Not parallel: it compares goroutine counts.
func TestRunReturnsWhenContextEndsWithoutLeaks(t *testing.T) {
	pool := pgtest.New(t)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	baseline := runtime.NumGoroutine()

	l, err := notify.NewListener(pool, notify.Config{})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	sub, err := l.Subscribe("lifecycle")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	waitListening(t, l, "lifecycle", true)

	pidCtx, pidCancel := context.WithTimeout(t.Context(), waitTimeout)
	defer pidCancel()
	pid, err := notify.BackendPID(pidCtx, l)
	if err != nil {
		t.Fatalf("BackendPID: %v", err)
	}
	if err := l.Run(ctx); !errors.Is(err, notify.ErrRunning) {
		t.Errorf("second Run = %v, want ErrRunning", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return after cancel")
	}

	waitFor(t, "LISTEN backend to disconnect", func() bool {
		var alive bool
		err := pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)", int64(pid)).Scan(&alive)
		return err == nil && !alive
	})
	waitFor(t, "goroutines to return to baseline", func() bool {
		return runtime.NumGoroutine() <= baseline
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFullBufferDeliversGap(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	reader := newMetricReader(t)
	l := startListener(t, pool, notify.Config{Buffer: 2}, notify.WithMeterProvider(reader.provider))

	sub := subscribe(t, l, "overflow")
	waitListening(t, l, "overflow", true)

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, payload := range []string{"1", "2", "3"} {
		if _, err := tx.Exec(t.Context(), "SELECT pg_notify('overflow', $1)", payload); err != nil {
			t.Fatalf("pg_notify: %v", err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	reader.waitCounter(t, "notify.gaps", 1)

	if got := receive(t, sub); got != (notify.Notification{Gap: true}) {
		t.Errorf("first delivery after overflow = %+v, want Gap", got)
	}
	sendNotify(t, pool, "overflow", "after")
	if got := receive(t, sub); got.Payload != "after" {
		t.Errorf("delivery after Gap = %+v, want payload after", got)
	}
}

func TestReconnectDeliversGapToEverySubscriber(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	reader := newMetricReader(t)
	l := startListener(t, pool, notify.Config{}, notify.WithMeterProvider(reader.provider))

	first := subscribe(t, l, "first")
	second := subscribe(t, l, "second")
	waitListening(t, l, "first", true)
	waitListening(t, l, "second", true)

	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	pid, err := notify.BackendPID(ctx, l)
	if err != nil {
		t.Fatalf("BackendPID: %v", err)
	}
	var terminated bool
	if err := pool.QueryRow(ctx, "SELECT pg_terminate_backend($1)", int64(pid)).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("pg_terminate_backend(%d) = %v, %v", pid, terminated, err)
	}

	for _, sub := range []*notify.Subscription{first, second} {
		if got := receive(t, sub); got != (notify.Notification{Gap: true}) {
			t.Errorf("delivery after reconnect = %+v, want Gap", got)
		}
	}
	reader.waitCounter(t, "notify.reconnects", 1)
	reader.waitCounter(t, "notify.gaps", 2)

	newPID, err := notify.BackendPID(ctx, l)
	if err != nil || newPID == pid {
		t.Fatalf("BackendPID after reconnect = %d, %v; want a new backend", newPID, err)
	}
	sendNotify(t, pool, "second", "back")
	if got := receive(t, second); got.Payload != "back" {
		t.Errorf("delivery after reconnect = %+v, want payload back", got)
	}
}

type metricReader struct {
	reader   *sdkmetric.ManualReader
	provider *sdkmetric.MeterProvider
}

func newMetricReader(t *testing.T) metricReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return metricReader{reader: reader, provider: provider}
}

func (r metricReader) counter(t *testing.T, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			var total int64
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	return 0
}

func (r metricReader) waitCounter(t *testing.T, name string, want int64) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		got := r.counter(t, name)
		if got == want {
			return
		}
		if got > want || time.Now().After(deadline) {
			t.Fatalf("counter %s = %d, want %d", name, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestListenFollowsSubscriberCount(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	l := startListener(t, pool, notify.Config{})
	const channel = "Counted Channel"

	first := subscribe(t, l, channel)
	second := subscribe(t, l, channel)
	waitListening(t, l, channel, true)

	first.Close()
	keepAlive := subscribe(t, l, "keep-alive")
	waitListening(t, l, "keep-alive", true)
	waitListening(t, l, channel, true)

	second.Close()
	waitListening(t, l, channel, false)

	third := subscribe(t, l, channel)
	waitListening(t, l, channel, true)
	sendNotify(t, pool, channel, "again")
	if got := receive(t, third); got.Payload != "again" {
		t.Errorf("resubscribed channel received %+v, want payload again", got)
	}
	keepAlive.Close()
	waitListening(t, l, "keep-alive", false)
}

func TestCloseIsIdempotentAndClosesC(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	l := startListener(t, pool, notify.Config{})

	sub := subscribe(t, l, "closing")
	other := subscribe(t, l, "closing")
	sub.Close()
	sub.Close()

	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("C delivered a value after Close, want closed channel")
		}
	case <-time.After(waitTimeout):
		t.Fatal("C not closed after Close")
	}

	waitListening(t, l, "closing", true)
	sendNotify(t, pool, "closing", "still here")
	if got := receive(t, other); got.Payload != "still here" {
		t.Errorf("remaining subscriber received %+v, want payload still here", got)
	}
}

func TestNotifyReachesEverySubscriberOfChannel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		channel string
	}{
		{name: "plain", channel: "public.events"},
		{name: "needs quotes", channel: `My "Channel"; DROP`},
		{name: "63 bytes", channel: strings.Repeat("c", 63)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pool := pgtest.New(t)
			l := startListener(t, pool, notify.Config{})

			first := subscribe(t, l, tt.channel)
			second := subscribe(t, l, tt.channel)
			other := subscribe(t, l, "other")
			waitListening(t, l, tt.channel, true)
			waitListening(t, l, "other", true)

			sendNotify(t, pool, tt.channel, "hello")

			for _, sub := range []*notify.Subscription{first, second} {
				got := receive(t, sub)
				if got != (notify.Notification{Payload: "hello"}) {
					t.Errorf("received %+v, want payload hello", got)
				}
			}
			sendNotify(t, pool, "other", "marker")
			if got := receive(t, other); got.Payload != "marker" {
				t.Errorf("other subscriber received %+v first, want marker", got)
			}
		})
	}
}

func startListener(t *testing.T, pool *pgxpool.Pool, cfg notify.Config, opts ...notify.Option) *notify.Listener {
	t.Helper()
	l, err := notify.NewListener(pool, cfg, opts...)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v, want nil", err)
			}
		case <-time.After(waitTimeout):
			t.Error("Run did not return after cancel")
		}
	})
	return l
}

func subscribe(t *testing.T, l *notify.Listener, channel string) *notify.Subscription {
	t.Helper()
	sub, err := l.Subscribe(channel)
	if err != nil {
		t.Fatalf("Subscribe(%q): %v", channel, err)
	}
	t.Cleanup(sub.Close)
	return sub
}

func waitListening(t *testing.T, l *notify.Listener, channel string, want bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		channels, err := notify.ListeningChannels(ctx, l)
		cancel()
		if err == nil && slices.Contains(channels, channel) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("listening on %q never became %v (last: %v, %v)", channel, want, channels, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sendNotify(t *testing.T, pool *pgxpool.Pool, channel, payload string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), "SELECT pg_notify($1, $2)", channel, payload); err != nil {
		t.Fatalf("pg_notify(%q): %v", channel, err)
	}
}

func receive(t *testing.T, sub *notify.Subscription) notify.Notification {
	t.Helper()
	select {
	case n, ok := <-sub.C():
		if !ok {
			t.Fatal("subscription channel closed")
		}
		return n
	case <-time.After(waitTimeout):
		t.Fatal("no notification received")
		return notify.Notification{}
	}
}
