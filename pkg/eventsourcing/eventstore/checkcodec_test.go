package eventstore_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// recordingTB captures failures so CheckCodec's own failures can be asserted.
type recordingTB struct {
	testing.TB
	errors []string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := eventstore.NewCodec()
			tt.register(c)
			rec := &recordingTB{TB: t}

			eventstore.CheckCodec(rec, c)

			if len(tt.wantErrors) == 0 {
				if len(rec.errors) > 0 {
					t.Fatalf("CheckCodec failed: %v", rec.errors)
				}
				return
			}
			if len(rec.errors) == 0 {
				t.Fatal("CheckCodec did not fail")
			}
			all := strings.Join(rec.errors, "\n")
			for _, want := range tt.wantErrors {
				if !strings.Contains(all, want) {
					t.Errorf("CheckCodec errors %q do not mention %q", all, want)
				}
			}
		})
	}
}

func TestCheckCodecRoundTripsWithUpcasters(t *testing.T) {
	t.Parallel()

	c := eventstore.NewCodec()
	registerTaskTitled(c)
	rec := &recordingTB{TB: t}

	eventstore.CheckCodec(rec, c)

	if len(rec.errors) > 0 {
		t.Fatalf("CheckCodec failed: %v", rec.errors)
	}
}
