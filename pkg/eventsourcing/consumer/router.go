package consumer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// Handler handles one event. It runs with the Envelope's Tenant and
// Correlation in ctx. A returned error is retried, unless it is Permanent.
type Handler interface {
	Handle(ctx context.Context, env eventstore.Envelope) error
}

// HandlerFunc adapts a function to a Handler.
type HandlerFunc func(ctx context.Context, env eventstore.Envelope) error

// Handle calls f.
func (f HandlerFunc) Handle(ctx context.Context, env eventstore.Envelope) error {
	return f(ctx, env)
}

// Router is a Handler built from typed handlers registered with On. Its
// EventTypes are the Consumer's filter unless Config.EventTypes is set.
// Events of unrouted types are ignored.
type Router struct {
	codec  *eventstore.Codec
	routes map[string]func(ctx context.Context, env eventstore.Envelope) error
}

// NewRouter returns an empty Router for events registered on codec. It
// panics on a nil codec.
func NewRouter(codec *eventstore.Codec) *Router {
	if codec == nil {
		panic("consumer: NewRouter: nil codec")
	}
	return &Router{codec: codec, routes: make(map[string]func(context.Context, eventstore.Envelope) error)}
}

// On routes events of type E to fn, which receives the Envelope and the
// decoded, upcast event. It panics on a nil fn, when E is not registered on
// the Router's codec, or when E is already routed. It is meant to be called
// at startup.
func On[E aggregate.Event](r *Router, fn func(ctx context.Context, env eventstore.Envelope, e E) error) {
	var zero E
	eventType := zero.EventType()
	if fn == nil {
		panic(fmt.Sprintf("consumer: On %q: nil handler", eventType))
	}
	if _, err := r.codec.Encode(zero); errors.Is(err, eventstore.ErrUnknownEventType) {
		panic(fmt.Sprintf("consumer: On %T: %v", zero, err))
	}
	if _, ok := r.routes[eventType]; ok {
		panic(fmt.Sprintf("consumer: On %q: already routed", eventType))
	}
	r.routes[eventType] = func(ctx context.Context, env eventstore.Envelope) error {
		e, ok := env.Event.(E)
		if !ok {
			return Permanent(fmt.Errorf("event %s is %T, want %T", env.EventID, env.Event, zero))
		}
		return fn(ctx, env, e)
	}
}

// EventTypes returns the routed Event types, sorted.
func (r *Router) EventTypes() []string {
	return slices.Sorted(maps.Keys(r.routes))
}

// Handle calls the route for env's Event type and returns nil for an
// unrouted type.
func (r *Router) Handle(ctx context.Context, env eventstore.Envelope) error {
	route, ok := r.routes[env.EventType]
	if !ok {
		return nil
	}
	return route(ctx, env)
}
