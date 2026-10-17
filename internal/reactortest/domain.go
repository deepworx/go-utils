package reactortest

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

type orderID uuid.UUID

// placed is the event the test Reactors react to; Note scripts the
// reaction.
type placed struct {
	Note string `json:"note"`
}

func (placed) EventType() string { return "Test.Order.Placed" }

// poisoned encodes fine but never decodes.
type poisoned struct {
	Value string `json:"value"`
}

func (poisoned) EventType() string { return "Test.Order.Poisoned" }

func (p *poisoned) UnmarshalJSON([]byte) error { return errors.New("poisoned payload") }

var _ json.Unmarshaler = (*poisoned)(nil)

type order struct {
	aggregate.Base[orderID]
}

func (o *order) StreamType() string { return "Test.Order" }

func (o *order) Apply(aggregate.Event) {}

type shipmentID uuid.UUID

// requested is what a Reactor's command raises.
type requested struct {
	Order string `json:"order"`
}

func (requested) EventType() string { return "Test.Shipment.Requested" }

type shipment struct {
	aggregate.Base[shipmentID]
}

func (s *shipment) StreamType() string { return "Test.Shipment" }

func (s *shipment) Apply(aggregate.Event) {}

func newCodec() *eventstore.Codec {
	c := eventstore.NewCodec()
	eventstore.Register[placed](c)
	eventstore.Register[poisoned](c)
	eventstore.Register[requested](c)
	return c
}
