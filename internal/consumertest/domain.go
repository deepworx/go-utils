package consumertest

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

type itemID uuid.UUID

type added struct {
	Name string `json:"name"`
}

func (added) EventType() string { return "Test.Item.Added" }

type renamed struct {
	Name string `json:"name"`
}

func (renamed) EventType() string { return "Test.Item.Renamed" }

// poisoned encodes fine but never decodes.
type poisoned struct {
	Value string `json:"value"`
}

func (poisoned) EventType() string { return "Test.Item.Poisoned" }

func (p *poisoned) UnmarshalJSON([]byte) error { return errors.New("poisoned payload") }

var _ json.Unmarshaler = (*poisoned)(nil)

type item struct {
	aggregate.Base[itemID]
}

func (i *item) StreamType() string { return "Test.Item" }

func (i *item) Apply(aggregate.Event) {}

func newCodec() *eventstore.Codec {
	c := eventstore.NewCodec()
	eventstore.Register[added](c)
	eventstore.Register[renamed](c)
	eventstore.Register[poisoned](c)
	return c
}
