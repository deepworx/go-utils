package consumer_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

type noteID uuid.UUID

type noteWritten struct {
	Text string `json:"text"`
}

func (noteWritten) EventType() string { return "Test.Note.Written" }

type noteDeleted struct{}

func (noteDeleted) EventType() string { return "Test.Note.Deleted" }

type unregistered struct{}

func (unregistered) EventType() string { return "Test.Note.Unregistered" }

type note struct {
	aggregate.Base[noteID]
}

func (n *note) StreamType() string { return "Test.Note" }

func (n *note) Apply(aggregate.Event) {}

func newCodec() *eventstore.Codec {
	c := eventstore.NewCodec()
	eventstore.Register[noteWritten](c)
	eventstore.Register[noteDeleted](c)
	return c
}

var quiet = consumer.WithLogger(slog.New(slog.DiscardHandler))

type env struct {
	codec *eventstore.Codec
	uow   *eventstore.InMemoryUnitOfWork
	notes *eventstore.Repository[noteID, note, *note]
	rt    *consumer.InMemoryRuntime
}

func newEnv() env {
	codec := newCodec()
	store := eventstore.NewInMemoryStore(codec)
	uow := eventstore.NewInMemoryUnitOfWork()
	return env{
		codec: codec,
		uow:   uow,
		notes: eventstore.NewRepository[noteID, note](eventstore.RepositoryConfig{Store: store, UnitOfWork: uow}),
		rt:    consumer.NewInMemoryRuntime(consumer.InMemoryRuntimeConfig{Store: store}),
	}
}

func writerCtx() context.Context {
	return ctxutil.WithActor(ctxutil.WithTenantID(context.Background(), "t1"), "tester")
}

func (e env) write(t *testing.T, ctx context.Context, text string) eventstore.CommandResult {
	t.Helper()
	res, err := e.notes.Create(ctx, noteID(uuid.Must(uuid.NewV7())), func(_ context.Context, n *note) error {
		aggregate.Raise(n, noteWritten{Text: text})
		return nil
	})
	if err != nil {
		t.Fatalf("write %q: %v", text, err)
	}
	return res
}

func noop(context.Context, eventstore.Envelope) error { return nil }

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	valid := consumer.Config{Name: "c", Mode: consumer.Transactional, Handler: consumer.HandlerFunc(noop)}
	tests := []struct {
		name string
		cfg  func(c *consumer.Config)
		opts []consumer.Option
	}{
		{name: "EmptyName", cfg: func(c *consumer.Config) { c.Name = "" }},
		{name: "NilHandler", cfg: func(c *consumer.Config) { c.Handler = nil }},
		{name: "ZeroMode", cfg: func(c *consumer.Config) { c.Mode = 0 }},
		{name: "EmptyEventType", cfg: func(c *consumer.Config) { c.EventTypes = []string{""} }},
		{name: "BatchSize", opts: []consumer.Option{consumer.WithBatchSize(0)}},
		{name: "PollInterval", opts: []consumer.Option{consumer.WithPollInterval(0)}},
		{name: "Retry", opts: []consumer.Option{consumer.WithRetry(consumer.Retry{MaxAttempts: 0, Base: time.Second, Max: time.Second})}},
		{name: "RetryMaxBelowBase", opts: []consumer.Option{consumer.WithRetry(consumer.Retry{MaxAttempts: 2, Base: time.Second, Max: time.Millisecond})}},
		{name: "NilLogger", opts: []consumer.Option{consumer.WithLogger(nil)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			_, err := consumer.New(newEnv().rt, cfg, tt.opts...)
			if !errors.Is(err, consumer.ErrInvalidConfig) {
				t.Fatalf("New = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestRouter(t *testing.T) {
	t.Parallel()
	codec := newCodec()
	r := consumer.NewRouter(codec)
	var got []string
	consumer.On(r, func(_ context.Context, _ eventstore.Envelope, e noteWritten) error {
		got = append(got, e.Text)
		return nil
	})
	consumer.On(r, func(context.Context, eventstore.Envelope, noteDeleted) error { return nil })

	if types := r.EventTypes(); !slices.Equal(types, []string{"Test.Note.Deleted", "Test.Note.Written"}) {
		t.Errorf("EventTypes = %v", types)
	}
	ctx := context.Background()
	if err := r.Handle(ctx, eventstore.Envelope{EventType: "Test.Note.Written", Event: noteWritten{Text: "hi"}}); err != nil {
		t.Fatalf("Handle routed: %v", err)
	}
	if err := r.Handle(ctx, eventstore.Envelope{EventType: "Test.Other"}); err != nil {
		t.Errorf("Handle unrouted = %v, want nil", err)
	}
	if !slices.Equal(got, []string{"hi"}) {
		t.Errorf("routed events = %v, want [hi]", got)
	}
}

func TestOnPanics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		on   func(r *consumer.Router)
	}{
		{"NilHandler", func(r *consumer.Router) { consumer.On[noteWritten](r, nil) }},
		{"Unregistered", func(r *consumer.Router) {
			consumer.On(r, func(context.Context, eventstore.Envelope, unregistered) error { return nil })
		}},
		{"Duplicate", func(r *consumer.Router) {
			consumer.On(r, func(context.Context, eventstore.Envelope, noteWritten) error { return nil })
			consumer.On(r, func(context.Context, eventstore.Envelope, noteWritten) error { return nil })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("On did not panic")
				}
			}()
			tt.on(consumer.NewRouter(newCodec()))
		})
	}
}

func TestPermanentNil(t *testing.T) {
	t.Parallel()
	if err := consumer.Permanent(nil); err != nil {
		t.Errorf("Permanent(nil) = %v, want nil", err)
	}
	cause := errors.New("cause")
	if err := consumer.Permanent(cause); !errors.Is(err, cause) {
		t.Errorf("Permanent(cause) = %v, want it to wrap cause", err)
	}
}

// An event committed after an earlier-started Command transaction stays
// hidden until that transaction ends, so the Cursor never passes an event
// that commits later with a smaller Cursor.
func TestInMemoryHidesEventsBehindOpenTransaction(t *testing.T) {
	t.Parallel()
	e := newEnv()
	var seen []string
	r := consumer.NewRouter(e.codec)
	consumer.On(r, func(_ context.Context, _ eventstore.Envelope, n noteWritten) error {
		seen = append(seen, n.Text)
		return nil
	})
	c, err := consumer.New(e.rt, consumer.Config{Name: "order", Mode: consumer.Transactional, Handler: r}, quiet)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	early := noteID(uuid.Must(uuid.NewV7()))
	appended := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- e.uow.Execute(writerCtx(), uuid.UUID(early), func(ctx context.Context) error {
			_, err := e.notes.Create(ctx, early, func(_ context.Context, n *note) error {
				aggregate.Raise(n, noteWritten{Text: "early"})
				return nil
			})
			close(appended)
			<-release
			return err
		})
	}()
	<-appended
	e.write(t, writerCtx(), "late")

	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("seen %v while an earlier transaction is open, want nothing", seen)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("early transaction: %v", err)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !slices.Equal(seen, []string{"early", "late"}) {
		t.Errorf("seen %v, want [early late]", seen)
	}
}

func TestEventSpans(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		on    bool
		spans []string
	}{
		{"Off", false, []string{"process spans"}},
		{"On", true, []string{"handle Test.Note.Written", "process spans"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv()
			e.write(t, writerCtx(), "a")
			rec := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
			c, err := consumer.New(e.rt,
				consumer.Config{Name: "spans", Mode: consumer.OwnTransaction, Handler: consumer.HandlerFunc(noop)},
				quiet, consumer.WithTracerProvider(tp), consumer.WithEventSpans(tt.on))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := c.Drain(context.Background()); err != nil {
				t.Fatalf("Drain: %v", err)
			}
			var names []string
			for _, s := range rec.Ended() {
				names = append(names, s.Name())
				if s.Name() == "process spans" && s.SpanKind() != trace.SpanKindConsumer {
					t.Errorf("batch span kind = %v, want consumer", s.SpanKind())
				}
			}
			if !slices.Equal(names, tt.spans) {
				t.Errorf("spans = %v, want %v", names, tt.spans)
			}
		})
	}
}
