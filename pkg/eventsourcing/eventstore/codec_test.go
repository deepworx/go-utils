package eventstore_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

type taskCreated struct {
	Title string `json:"title"`
}

func (taskCreated) EventType() string { return "task.created" }

type taskRenamed struct {
	Title string `json:"title"`
}

func (taskRenamed) EventType() string { return "task.renamed" }

type ptrEvent struct{}

func (*ptrEvent) EventType() string { return "ptr.event" }

type emptyTypeEvent struct{}

func (emptyTypeEvent) EventType() string { return "" }

type impostorCreated struct {
	Name string `json:"name"`
}

func (impostorCreated) EventType() string { return "task.created" }

func noop(b []byte) ([]byte, error) { return b, nil }

func TestRegisterPanics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		register func(c *eventstore.Codec)
	}{
		{
			name:     "pointer event",
			register: func(c *eventstore.Codec) { eventstore.Register[*ptrEvent](c) },
		},
		{
			name:     "empty event type",
			register: func(c *eventstore.Codec) { eventstore.Register[emptyTypeEvent](c) },
		},
		{
			name: "duplicate event type",
			register: func(c *eventstore.Codec) {
				eventstore.Register[taskCreated](c)
				eventstore.Register[taskCreated](c)
			},
		},
		{
			name: "duplicate event type on another struct",
			register: func(c *eventstore.Codec) {
				eventstore.Register[taskCreated](c)
				eventstore.Register[impostorCreated](c)
			},
		},
		{
			name:     "nil upcaster",
			register: func(c *eventstore.Codec) { eventstore.Register[taskCreated](c, noop, nil) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatal("Register did not panic")
				}
			}()
			tt.register(eventstore.NewCodec())
		})
	}
}

func TestEncodeUsesSchemaVersionFromUpcasterCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		upcasters  []eventstore.Upcaster
		wantSchema int
	}{
		{name: "no upcasters", upcasters: nil, wantSchema: 1},
		{name: "one upcaster", upcasters: []eventstore.Upcaster{noop}, wantSchema: 2},
		{name: "three upcasters", upcasters: []eventstore.Upcaster{noop, noop, noop}, wantSchema: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := eventstore.NewCodec()
			eventstore.Register[taskCreated](c, tt.upcasters...)

			got, err := c.Encode(taskCreated{Title: "write tests"})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			want := eventstore.Payload{
				EventType:     "task.created",
				SchemaVersion: tt.wantSchema,
				Data:          []byte(`{"title":"write tests"}`),
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Encode = %+v, want %+v", got, want)
			}
		})
	}
}

// taskTitled is at Schema version 4: v1 stored "name", v2 renamed it to
// "title", v3 added "priority" and v4 brackets the title, so every step
// leaves a visible trace.
type taskTitled struct {
	Title    string `json:"title"`
	Priority int    `json:"priority"`
}

func (taskTitled) EventType() string { return "task.titled" }

func registerTaskTitled(c *eventstore.Codec) {
	eventstore.Register[taskTitled](c,
		eventstore.UpcastObject(func(m map[string]any) error {
			m["title"] = m["name"]
			delete(m, "name")
			return nil
		}),
		eventstore.UpcastObject(func(m map[string]any) error {
			m["priority"] = 3
			return nil
		}),
		eventstore.UpcastObject(func(m map[string]any) error {
			m["title"] = fmt.Sprintf("[%s]", m["title"])
			return nil
		}),
	)
}

func TestDecodeRunsUpcasterChainFromStoredVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload eventstore.Payload
		want    taskTitled
	}{
		{
			name:    "v1 runs all three upcasters",
			payload: eventstore.Payload{EventType: "task.titled", SchemaVersion: 1, Data: []byte(`{"name":"ship"}`)},
			want:    taskTitled{Title: "[ship]", Priority: 3},
		},
		{
			name:    "v3 runs only the last upcaster",
			payload: eventstore.Payload{EventType: "task.titled", SchemaVersion: 3, Data: []byte(`{"title":"ship","priority":1}`)},
			want:    taskTitled{Title: "[ship]", Priority: 1},
		},
		{
			name:    "current version runs no upcaster",
			payload: eventstore.Payload{EventType: "task.titled", SchemaVersion: 4, Data: []byte(`{"title":"ship","priority":1}`)},
			want:    taskTitled{Title: "ship", Priority: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := eventstore.NewCodec()
			registerTaskTitled(c)

			got, err := c.Decode(tt.payload)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got != tt.want {
				t.Errorf("Decode = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestDecodeErrors(t *testing.T) {
	t.Parallel()

	errUpcast := errors.New("upcast failed")
	tests := []struct {
		name    string
		payload eventstore.Payload
		wantErr error
	}{
		{
			name:    "stored version newer than registered",
			payload: eventstore.Payload{EventType: "task.renamed", SchemaVersion: 3, Data: []byte(`{}`)},
			wantErr: eventstore.ErrNewerSchemaVersion,
		},
		{
			name:    "stored version zero",
			payload: eventstore.Payload{EventType: "task.renamed", SchemaVersion: 0, Data: []byte(`{}`)},
			wantErr: eventstore.ErrInvalidSchemaVersion,
		},
		{
			name:    "stored version negative",
			payload: eventstore.Payload{EventType: "task.renamed", SchemaVersion: -1, Data: []byte(`{}`)},
			wantErr: eventstore.ErrInvalidSchemaVersion,
		},
		{
			name:    "unregistered event type",
			payload: eventstore.Payload{EventType: "task.deleted", SchemaVersion: 1, Data: []byte(`{}`)},
			wantErr: eventstore.ErrUnknownEventType,
		},
		{
			name:    "failing upcaster",
			payload: eventstore.Payload{EventType: "task.renamed", SchemaVersion: 1, Data: []byte(`{}`)},
			wantErr: errUpcast,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := eventstore.NewCodec()
			eventstore.Register[taskRenamed](c, func([]byte) ([]byte, error) { return nil, errUpcast })

			_, err := c.Decode(tt.payload)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Decode error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeIsLenient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want taskTitled
	}{
		{name: "unknown field is ignored", data: `{"title":"ship","priority":2,"owner":"ana"}`, want: taskTitled{Title: "ship", Priority: 2}},
		{name: "missing field is zero", data: `{"title":"ship"}`, want: taskTitled{Title: "ship"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := eventstore.NewCodec()
			eventstore.Register[taskTitled](c)

			got, err := c.Decode(eventstore.Payload{EventType: "task.titled", SchemaVersion: 1, Data: []byte(tt.data)})
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got != tt.want {
				t.Errorf("Decode = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestEncodeErrors(t *testing.T) {
	t.Parallel()

	c := eventstore.NewCodec()
	eventstore.Register[taskCreated](c)

	tests := []struct {
		name  string
		event aggregate.Event
	}{
		{name: "unregistered event type", event: taskRenamed{}},
		{name: "other Go type with a registered event type", event: impostorCreated{}},
		{name: "pointer to registered event", event: &taskCreated{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := c.Encode(tt.event); !errors.Is(err, eventstore.ErrUnknownEventType) {
				t.Errorf("Encode error = %v, want %v", err, eventstore.ErrUnknownEventType)
			}
		})
	}
}

func TestCodecsDoNotShareRegistrations(t *testing.T) {
	t.Parallel()

	first := eventstore.NewCodec()
	second := eventstore.NewCodec()
	eventstore.Register[taskCreated](first)
	eventstore.Register[taskCreated](second, noop)

	if _, err := eventstore.NewCodec().Encode(taskCreated{}); !errors.Is(err, eventstore.ErrUnknownEventType) {
		t.Errorf("fresh codec Encode error = %v, want %v", err, eventstore.ErrUnknownEventType)
	}
	var got []int
	for _, c := range []*eventstore.Codec{first, second} {
		p, err := c.Encode(taskCreated{})
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		got = append(got, p.SchemaVersion)
	}
	if want := []int{1, 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("schema versions = %v, want %v", got, want)
	}
}

func TestUpcastReturnsCurrentShapeForTooling(t *testing.T) {
	t.Parallel()

	c := eventstore.NewCodec()
	registerTaskTitled(c)

	got, err := c.Upcast(eventstore.Payload{EventType: "task.titled", SchemaVersion: 1, Data: []byte(`{"name":"ship"}`)})
	if err != nil {
		t.Fatalf("Upcast: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("unmarshal upcast: %v", err)
	}
	want := map[string]any{"title": "[ship]", "priority": float64(3)}
	if !reflect.DeepEqual(obj, want) {
		t.Errorf("Upcast = %v, want %v", obj, want)
	}
}
