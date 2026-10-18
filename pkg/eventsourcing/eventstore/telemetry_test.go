package eventstore_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

type taskRepository = eventstore.Repository[taskID, task, *task]

func newRecorder(t *testing.T) (*tracetest.SpanRecorder, trace.TracerProvider) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return rec, tp
}

func newInMemoryTaskRepository(opts ...eventstore.RepositoryOption) (*taskRepository, *eventstore.InMemoryStore) {
	codec := eventstore.NewCodec()
	eventstore.Register[taskCreated](codec)
	eventstore.Register[taskRenamed](codec)
	store := eventstore.NewInMemoryStore(codec)
	repo := eventstore.NewRepository[taskID, task](eventstore.RepositoryConfig{
		Store:      store,
		UnitOfWork: eventstore.NewInMemoryUnitOfWork(),
	}, opts...)
	return repo, store
}

func renameTask(title string) func(context.Context, *task) error {
	return func(_ context.Context, t *task) error {
		aggregate.Raise(t, taskRenamed{Title: title})
		aggregate.Raise(t, taskRenamed{Title: title + "!"})
		return nil
	}
}

func spanNamed(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no span %q", name)
	return nil
}

func attrValue(s sdktrace.ReadOnlySpan, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestCommandSpans(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		run        func(ctx context.Context, repo *taskRepository, id taskID) error
		command    string
		eventTypes []string
	}{
		{
			name: "create",
			run: func(ctx context.Context, repo *taskRepository, id taskID) error {
				_, err := repo.Create(ctx, id, createTask("a"))
				return err
			},
			command:    "create Test.Task",
			eventTypes: []string{"task.created"},
		},
		{
			name: "update",
			run: func(ctx context.Context, repo *taskRepository, id taskID) error {
				if _, err := repo.Create(ctx, id, createTask("a")); err != nil {
					return err
				}
				_, err := repo.Update(ctx, id, renameTask("b"))
				return err
			},
			command:    "update Test.Task",
			eventTypes: []string{"task.renamed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, tp := newRecorder(t)
			repo, _ := newInMemoryTaskRepository(eventstore.WithTracerProvider(tp))
			if err := tt.run(userCtx("t1"), repo, newTaskID()); err != nil {
				t.Fatalf("command: %v", err)
			}

			spans := rec.Ended()
			command := spanNamed(t, spans, tt.command)
			if command.SpanKind() != trace.SpanKindInternal {
				t.Errorf("%s kind = %v, want internal", tt.command, command.SpanKind())
			}
			got, _ := attrValue(command, "eventsourcing.event_types")
			if !slices.Equal(got.AsStringSlice(), tt.eventTypes) {
				t.Errorf("event_types = %v, want %v", got.AsStringSlice(), tt.eventTypes)
			}

			var send sdktrace.ReadOnlySpan
			for _, s := range spans {
				if s.Name() == "send Test.Task" && s.Parent().SpanID() == command.SpanContext().SpanID() {
					send = s
				}
			}
			if send == nil {
				t.Fatalf("no send Test.Task span under %s", tt.command)
			}
			if send.SpanKind() != trace.SpanKindProducer {
				t.Errorf("send kind = %v, want producer", send.SpanKind())
			}
		})
	}
}

func TestCommandSpanRecordsErrorType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		run     func(ctx context.Context, repo *taskRepository) error
		want    error
		errType string
	}{
		{
			name: "already exists",
			run: func(ctx context.Context, repo *taskRepository) error {
				id := newTaskID()
				if _, err := repo.Create(ctx, id, createTask("a")); err != nil {
					return err
				}
				_, err := repo.Create(ctx, id, createTask("b"))
				return err
			},
			want:    eventstore.ErrAlreadyExists,
			errType: "already_exists",
		},
		{
			name: "command id reused",
			run: func(ctx context.Context, repo *taskRepository) error {
				cmd := eventstore.WithCommandID(uuid.New())
				if _, err := repo.Create(ctx, newTaskID(), createTask("a"), cmd); err != nil {
					return err
				}
				_, err := repo.Create(ctx, newTaskID(), createTask("b"), cmd)
				return err
			},
			want:    eventstore.ErrCommandIDReused,
			errType: "command_id_reused",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, tp := newRecorder(t)
			repo, _ := newInMemoryTaskRepository(eventstore.WithTracerProvider(tp))
			if err := tt.run(userCtx("t1"), repo); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}

			spans := rec.Ended()
			failed := spans[len(spans)-1]
			if failed.Name() != "create Test.Task" {
				t.Fatalf("last span = %q, want create Test.Task", failed.Name())
			}
			if failed.Status().Code != codes.Error {
				t.Errorf("status = %v, want error", failed.Status().Code)
			}
			if got, _ := attrValue(failed, "error.type"); got.AsString() != tt.errType {
				t.Errorf("error.type = %q, want %s", got.AsString(), tt.errType)
			}
		})
	}
}

func TestCommandSpanMarksReplay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		existed bool
		run     func(ctx context.Context, repo *taskRepository, id taskID, cmd eventstore.CommandOption) error
		span    string
	}{
		{
			name: "create",
			run: func(ctx context.Context, repo *taskRepository, id taskID, cmd eventstore.CommandOption) error {
				_, err := repo.Create(ctx, id, createTask("a"), cmd)
				return err
			},
			span: "create Test.Task",
		},
		{
			name:    "update",
			existed: true,
			run: func(ctx context.Context, repo *taskRepository, id taskID, cmd eventstore.CommandOption) error {
				_, err := repo.Update(ctx, id, renameTask("b"), cmd)
				return err
			},
			span: "update Test.Task",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, tp := newRecorder(t)
			repo, _ := newInMemoryTaskRepository(eventstore.WithTracerProvider(tp))
			ctx := userCtx("t1")
			id := newTaskID()
			if tt.existed {
				if _, err := repo.Create(ctx, id, createTask("a")); err != nil {
					t.Fatalf("Create: %v", err)
				}
			}
			cmd := eventstore.WithCommandID(uuid.New())
			for range 2 {
				if err := tt.run(ctx, repo, id, cmd); err != nil {
					t.Fatalf("command: %v", err)
				}
			}

			var replayed []bool
			for _, s := range rec.Ended() {
				if s.Name() == tt.span {
					v, ok := attrValue(s, "eventsourcing.command.replayed")
					replayed = append(replayed, ok && v.AsBool())
				}
			}
			if !slices.Equal(replayed, []bool{false, true}) {
				t.Errorf("replayed on %s spans = %v, want only on the repeat", tt.span, replayed)
			}
		})
	}
}

// traceBackend opens a task Repository and reads back the stored
// trace_context of every event of a Stream, NULL as "".
type traceBackend struct {
	repository    func(t *testing.T, opts ...eventstore.RepositoryOption) *taskRepository
	traceContexts func(t *testing.T, tenant string, id taskID) []string
}

func traceBackends() map[string]func(t *testing.T) traceBackend {
	return map[string]func(t *testing.T) traceBackend{
		"in-memory": func(*testing.T) traceBackend {
			var store *eventstore.InMemoryStore
			return traceBackend{
				repository: func(_ *testing.T, opts ...eventstore.RepositoryOption) *taskRepository {
					var repo *taskRepository
					repo, store = newInMemoryTaskRepository(opts...)
					return repo
				},
				traceContexts: func(_ *testing.T, tenant string, id taskID) []string {
					return eventstore.TraceContexts(store, tenant, uuid.UUID(id))
				},
			}
		},
		"postgres": func(t *testing.T) traceBackend {
			pool := pgtest.New(t)
			return traceBackend{
				repository: func(t *testing.T, opts ...eventstore.RepositoryOption) *taskRepository {
					codec := eventstore.NewCodec()
					eventstore.Register[taskCreated](codec)
					eventstore.Register[taskRenamed](codec)
					return eventstore.NewRepository[taskID, task](eventstore.RepositoryConfig{
						Store:      newPgStore(t, eventstore.PgStoreConfig{Pool: pool, Codec: codec}),
						UnitOfWork: newPgUnitOfWork(t, pool, ""),
					}, opts...)
				},
				traceContexts: func(t *testing.T, tenant string, id taskID) []string {
					rows, err := pool.Query(t.Context(), `SELECT COALESCE(trace_context, '') FROM public.events
						WHERE tenant_id = $1 AND stream_id = $2 ORDER BY stream_position`, tenant, uuid.UUID(id))
					if err != nil {
						t.Fatalf("query trace_context: %v", err)
					}
					got, err := pgx.CollectRows(rows, pgx.RowTo[string])
					if err != nil {
						t.Fatalf("collect trace_context: %v", err)
					}
					return got
				},
			}
		},
	}
}

func TestSendSpanContextIsStoredOnEvents(t *testing.T) {
	t.Parallel()
	for name, open := range traceBackends() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := open(t)
			rec, tp := newRecorder(t)
			repo := b.repository(t, eventstore.WithTracerProvider(tp))

			state, err := trace.ParseTraceState("vendor=secret")
			if err != nil {
				t.Fatalf("ParseTraceState: %v", err)
			}
			ctx := trace.ContextWithRemoteSpanContext(userCtx("t1"), trace.NewSpanContext(trace.SpanContextConfig{
				TraceID:    trace.TraceID{1},
				SpanID:     trace.SpanID{2},
				TraceFlags: trace.FlagsSampled,
				TraceState: state,
			}))
			member, err := baggage.NewMember("user", "alice")
			if err != nil {
				t.Fatalf("baggage.NewMember: %v", err)
			}
			bag, err := baggage.New(member)
			if err != nil {
				t.Fatalf("baggage.New: %v", err)
			}
			ctx = baggage.ContextWithBaggage(ctx, bag)

			id := newTaskID()
			if _, err := repo.Create(ctx, id, createTask("a")); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, err := repo.Update(ctx, id, renameTask("b")); err != nil {
				t.Fatalf("Update: %v", err)
			}

			var sends []trace.SpanContext
			for _, s := range rec.Ended() {
				if s.Name() == "send Test.Task" {
					sends = append(sends, s.SpanContext())
				}
			}
			if len(sends) != 2 {
				t.Fatalf("send spans = %d, want 2", len(sends))
			}
			want := []trace.SpanContext{sends[0], sends[1], sends[1]}
			stored := b.traceContexts(t, "t1", id)
			if len(stored) != len(want) {
				t.Fatalf("stored %d trace contexts, want %d", len(stored), len(want))
			}
			for i, tc := range stored {
				if strings.Contains(tc, "secret") || strings.Contains(tc, "alice") {
					t.Errorf("event %d stores tracestate or baggage: %q", i+1, tc)
				}
				got := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(
					context.Background(), propagation.MapCarrier{"traceparent": tc}))
				if got.TraceID() != want[i].TraceID() || got.SpanID() != want[i].SpanID() || got.TraceFlags() != want[i].TraceFlags() {
					t.Errorf("event %d trace_context %q, want send span %s/%s", i+1, tc, want[i].TraceID(), want[i].SpanID())
				}
			}
		})
	}
}

// TestNoopWithoutProviders relies on the OTel globals staying unset in this
// test binary.
func TestNoopWithoutProviders(t *testing.T) {
	t.Parallel()
	for name, open := range traceBackends() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := open(t)
			repo := b.repository(t)
			ctx := userCtx("t1")
			id := newTaskID()
			if _, err := repo.Create(ctx, id, createTask("a")); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, err := repo.Update(ctx, id, renameTask("b")); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if _, err := repo.Load(ctx, id); err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := b.traceContexts(t, "t1", id); !slices.Equal(got, []string{"", "", ""}) {
				t.Errorf("trace contexts = %q, want all NULL", got)
			}
		})
	}
}

func TestHistogramLabels(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	repo, _ := newInMemoryTaskRepository(eventstore.WithMeterProvider(mp))

	ctx := userCtx("t1")
	id := newTaskID()
	if _, err := repo.Create(ctx, id, createTask("a")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cmd := eventstore.WithCommandID(uuid.New())
	for range 2 {
		if _, err := repo.Update(ctx, id, renameTask("b"), cmd); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	if _, err := repo.Load(ctx, id); err != nil {
		t.Fatalf("Load: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	type series struct {
		name     string
		replayed string
	}
	type summary struct {
		count uint64
		sum   int64
	}
	got := map[series]summary{}
	add := func(name string, attrs attribute.Set, count uint64, sum int64) {
		streamType, _ := attrs.Value("eventsourcing.stream_type")
		replayed, hasReplayed := attrs.Value("eventsourcing.command.replayed")
		wantLen := 1
		if hasReplayed {
			wantLen = 2
		}
		if streamType.AsString() != "Test.Task" || attrs.Len() != wantLen {
			t.Errorf("%s labels = %v, want the stream type and at most replayed", name, attrs.ToSlice())
		}
		k := series{name: name, replayed: replayed.String()}
		got[k] = summary{count: got[k].count + count, sum: got[k].sum + sum}
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					add(m.Name, dp.Attributes, dp.Count, 0)
				}
			case metricdata.Histogram[int64]:
				for _, dp := range data.DataPoints {
					add(m.Name, dp.Attributes, dp.Count, dp.Sum)
				}
			}
		}
	}

	// The repeated Update replays without loading the Aggregate.
	want := map[series]summary{
		{name: "eventsourcing.command.duration", replayed: "false"}: {count: 2},
		{name: "eventsourcing.command.duration", replayed: "true"}:  {count: 1},
		{name: "eventsourcing.aggregate.load.duration"}:             {count: 2},
		{name: "eventsourcing.aggregate.load.events"}:               {count: 2, sum: 1 + 3},
	}
	if len(got) != len(want) {
		t.Errorf("series = %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%+v = %+v, want %+v", k, got[k], w)
		}
	}
}
