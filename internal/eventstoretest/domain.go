package eventstoretest

import (
	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

type accountID uuid.UUID

type opened struct {
	Owner string `json:"owner"`
}

func (opened) EventType() string { return "Test.Account.Opened" }

type deposited struct {
	Amount int `json:"amount"`
}

func (deposited) EventType() string { return "Test.Account.Deposited" }

// labelled is the current shape of an event whose v1 stored "name".
type labelled struct {
	Label string `json:"label"`
}

func (labelled) EventType() string { return "Test.Account.Labelled" }

// labelledV1 is the stored v1 shape of labelled.
type labelledV1 struct {
	Name string `json:"name"`
}

func (labelledV1) EventType() string { return "Test.Account.Labelled" }

type account struct {
	aggregate.Base[accountID]
	owner   string
	balance int
	label   string
}

func (a *account) StreamType() string { return "Test.Account" }

func (a *account) Apply(e aggregate.Event) {
	switch e := e.(type) {
	case opened:
		a.owner = e.Owner
	case deposited:
		a.balance += e.Amount
	case labelled:
		a.label = e.Label
	}
}

func (a *account) open(owner string) { aggregate.Raise(a, opened{Owner: owner}) }

func (a *account) deposit(amount int) { aggregate.Raise(a, deposited{Amount: amount}) }

type orderID uuid.UUID

type placed struct{}

func (placed) EventType() string { return "Test.Order.Placed" }

type order struct {
	aggregate.Base[orderID]
}

func (o *order) StreamType() string { return "Test.Order" }

func (o *order) Apply(aggregate.Event) {}

func (o *order) place() { aggregate.Raise(o, placed{}) }

func newCodec() *eventstore.Codec {
	c := eventstore.NewCodec()
	eventstore.Register[opened](c)
	eventstore.Register[deposited](c)
	eventstore.Register[labelled](c, eventstore.UpcastObject(func(m map[string]any) error {
		m["label"] = m["name"]
		delete(m, "name")
		return nil
	}))
	eventstore.Register[placed](c)
	return c
}

func newCodecV1() *eventstore.Codec {
	c := eventstore.NewCodec()
	eventstore.Register[opened](c)
	eventstore.Register[labelledV1](c)
	return c
}

// raiseV1Label raises the stored v1 shape, as code from before the
// Upcaster did.
func raiseV1Label(a *account, name string) { aggregate.Raise(a, labelledV1{Name: name}) }
