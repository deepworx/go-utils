package eventstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
)

// Upcaster lifts one stored event payload from one Schema version to the
// next. It receives exactly one payload and returns exactly one.
type Upcaster func(data []byte) ([]byte, error)

// Payload is an event as it is stored: its Event type, the Schema version it
// was written with, and its JSON bytes.
type Payload struct {
	EventType     string
	SchemaVersion int
	Data          []byte
}

// Codec turns domain events into JSON payloads and back, upcasting stored
// payloads to the current Schema version on every read. Each Codec has its
// own registrations; there is no global registry. A Codec is safe for
// concurrent use.
type Codec struct {
	mu    sync.RWMutex
	types map[string]registration
}

type registration struct {
	typ       reflect.Type
	upcasters []Upcaster
	decode    func(data []byte) (aggregate.Event, error)
}

func (r registration) schemaVersion() int { return len(r.upcasters) + 1 }

// NewCodec returns a Codec without registrations.
func NewCodec() *Codec {
	return &Codec{types: make(map[string]registration)}
}

// Register makes the event type E known to c. Its Schema version is
// len(upcasters)+1: upcasters[i] lifts a payload from version i+1 to i+2.
//
// Register panics if E is a pointer type, if E's Event type is empty or
// already registered on c, or if an upcaster is nil. It is meant to be called
// at startup.
func Register[E aggregate.Event](c *Codec, upcasters ...Upcaster) {
	typ := reflect.TypeFor[E]()
	if typ.Kind() == reflect.Pointer {
		panic(fmt.Sprintf("eventstore: register %s: event must not be a pointer type", typ))
	}
	var zero E
	eventType := zero.EventType()
	if eventType == "" {
		panic(fmt.Sprintf("eventstore: register %s: empty event type", typ))
	}
	for i, u := range upcasters {
		if u == nil {
			panic(fmt.Sprintf("eventstore: register %q: upcaster %d is nil", eventType, i))
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.types[eventType]; ok {
		panic(fmt.Sprintf("eventstore: register %s: event type %q already registered by %s", typ, eventType, existing.typ))
	}
	c.types[eventType] = registration{
		typ:       typ,
		upcasters: append([]Upcaster(nil), upcasters...),
		decode: func(data []byte) (aggregate.Event, error) {
			var e E
			if err := json.Unmarshal(data, &e); err != nil {
				return nil, err
			}
			return e, nil
		},
	}
}

// Encode marshals e at its current Schema version. It fails with
// ErrUnknownEventType if e's Event type is not registered for e's Go type.
func (c *Codec) Encode(e aggregate.Event) (Payload, error) {
	eventType := e.EventType()
	reg, err := c.registration(eventType)
	if err != nil {
		return Payload{}, fmt.Errorf("encode: %w", err)
	}
	if got := reflect.TypeOf(e); got != reg.typ {
		return Payload{}, fmt.Errorf("encode %s: %w: %q is registered for %s", got, ErrUnknownEventType, eventType, reg.typ)
	}
	data, err := json.Marshal(e)
	if err != nil {
		return Payload{}, fmt.Errorf("encode %q: %w", eventType, err)
	}
	return Payload{EventType: eventType, SchemaVersion: reg.schemaVersion(), Data: data}, nil
}

// Decode upcasts p to the current Schema version and unmarshals it into the
// registered event type. Decoding is lenient: unknown fields are ignored and
// missing fields stay zero.
func (c *Codec) Decode(p Payload) (aggregate.Event, error) {
	reg, data, err := c.upcast(p)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	e, err := reg.decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode %q: %w", p.EventType, err)
	}
	return e, nil
}

// Upcast lifts a stored payload from its Schema version to the current one
// and returns its JSON, so tooling sees today's shape.
//
// It fails with ErrUnknownEventType, ErrInvalidSchemaVersion for a version
// below 1, ErrNewerSchemaVersion for a version above the registered one, or
// the wrapped error of a failing Upcaster.
func (c *Codec) Upcast(p Payload) ([]byte, error) {
	_, data, err := c.upcast(p)
	if err != nil {
		return nil, fmt.Errorf("upcast: %w", err)
	}
	return data, nil
}

func (c *Codec) upcast(p Payload) (registration, []byte, error) {
	reg, err := c.registration(p.EventType)
	if err != nil {
		return registration{}, nil, err
	}
	current := reg.schemaVersion()
	switch {
	case p.SchemaVersion < 1:
		return registration{}, nil, fmt.Errorf("%q v%d: %w", p.EventType, p.SchemaVersion, ErrInvalidSchemaVersion)
	case p.SchemaVersion > current:
		return registration{}, nil, fmt.Errorf("%q v%d, registered v%d: %w", p.EventType, p.SchemaVersion, current, ErrNewerSchemaVersion)
	}
	data := p.Data
	for v := p.SchemaVersion; v < current; v++ {
		data, err = reg.upcasters[v-1](data)
		if err != nil {
			return registration{}, nil, fmt.Errorf("%q v%d to v%d: %w", p.EventType, v, v+1, err)
		}
	}
	return reg, data, nil
}

// UpcastObject returns an Upcaster that decodes the payload as a JSON object,
// lets edit change it in place and encodes it again. Numbers are decoded as
// json.Number, so large integers stay exact.
func UpcastObject(edit func(obj map[string]any) error) Upcaster {
	return func(data []byte) ([]byte, error) {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			return nil, fmt.Errorf("decode object: %w", err)
		}
		if obj == nil {
			return nil, errors.New("decode object: payload is null")
		}
		if err := edit(obj); err != nil {
			return nil, err
		}
		out, err := json.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("encode object: %w", err)
		}
		return out, nil
	}
}

func (c *Codec) registration(eventType string) (registration, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	reg, ok := c.types[eventType]
	if !ok {
		return registration{}, fmt.Errorf("%w: %q", ErrUnknownEventType, eventType)
	}
	return reg, nil
}
