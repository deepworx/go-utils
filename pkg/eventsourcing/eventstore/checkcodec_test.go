package eventstore_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

type lineItem struct {
	SKU      string            `json:"sku"`
	Quantity int               `json:"quantity"`
	Tags     map[string]string `json:"tags"`
}

type orderPlaced struct {
	OrderID  uuid.UUID       `json:"order_id"`
	Items    []lineItem      `json:"items"`
	Note     *string         `json:"note,omitempty"`
	PlacedAt time.Time       `json:"placed_at"`
	Rate     float64         `json:"rate"`
	Express  bool            `json:"express"`
	Codes    [2]uint8        `json:"codes"`
	Raw      json.RawMessage `json:"raw"`
	Ignored  string          `json:"-"`
}

func (orderPlaced) EventType() string { return "order.placed" }

type orderShipped struct {
	Carrier string
}

func (orderShipped) EventType() string { return "order.shipped" }

type nestedUntagged struct {
	Items []untaggedItem `json:"items"`
}

type untaggedItem struct {
	SKU string
}

func (nestedUntagged) EventType() string { return "order.nested" }

type stringBlocked struct {
	Reason string `json:"reason"`
}

// MarshalJSON loses Reason, so a round trip does not return the event.
func (stringBlocked) MarshalJSON() ([]byte, error) { return []byte(`{}`), nil }

func (stringBlocked) EventType() string { return "order.blocked" }

func TestCheckCodec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		register   func(c *eventstore.Codec)
		wantErrors []string
	}{
		{
			name: "correctly tagged events pass",
			register: func(c *eventstore.Codec) {
				eventstore.Register[orderPlaced](c)
				eventstore.Register[taskCreated](c)
			},
		},
		{
			name:     "untagged exported field fails",
			register: func(c *eventstore.Codec) { eventstore.Register[orderShipped](c) },
			wantErrors: []string{
				`"order.shipped"`,
				"Carrier",
			},
		},
		{
			name:     "untagged field in nested type fails",
			register: func(c *eventstore.Codec) { eventstore.Register[nestedUntagged](c) },
			wantErrors: []string{
				"Items[].SKU",
			},
		},
		{
			name:     "lossy round trip fails",
			register: func(c *eventstore.Codec) { eventstore.Register[stringBlocked](c) },
			wantErrors: []string{
				`"order.blocked"`,
				"round trip",
			},
		},
		{
			name: "every broken event type is reported",
			register: func(c *eventstore.Codec) {
				eventstore.Register[orderShipped](c)
				eventstore.Register[stringBlocked](c)
			},
			wantErrors: []string{
				`"order.shipped"`,
				"Carrier",
				`"order.blocked"`,
				"round trip",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := eventstore.NewCodec()
			tt.register(c)

			err := eventstore.CheckCodec(c)

			if len(tt.wantErrors) == 0 {
				if err != nil {
					t.Fatalf("CheckCodec() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("CheckCodec() = nil, want an error")
			}
			for _, want := range tt.wantErrors {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("CheckCodec() = %q, does not mention %q", err, want)
				}
			}
		})
	}
}

func TestCheckCodecRoundTripsWithUpcasters(t *testing.T) {
	t.Parallel()

	c := eventstore.NewCodec()
	registerTaskTitled(c)

	if err := eventstore.CheckCodec(c); err != nil {
		t.Fatalf("CheckCodec() = %v, want nil", err)
	}
}
