package eventstore_test

import (
	"errors"
	"testing"

	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

type amountBooked struct {
	Cents int64  `json:"cents"`
	Note  string `json:"note"`
}

func (amountBooked) EventType() string { return "amount.booked" }

func TestUpcastObjectKeepsLargeIntegersExact(t *testing.T) {
	t.Parallel()

	const above2pow53 int64 = 1<<53 + 1
	c := eventstore.NewCodec()
	eventstore.Register[amountBooked](c, eventstore.UpcastObject(func(m map[string]any) error {
		m["note"] = "migrated"
		return nil
	}))

	got, err := c.Decode(eventstore.Payload{
		EventType:     "amount.booked",
		SchemaVersion: 1,
		Data:          []byte(`{"cents":9007199254740993}`),
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := amountBooked{Cents: above2pow53, Note: "migrated"}
	if got != want {
		t.Errorf("Decode = %#v, want %#v", got, want)
	}
}

func TestUpcastObjectErrors(t *testing.T) {
	t.Parallel()

	errEdit := errors.New("edit failed")
	keep := func(map[string]any) error { return nil }
	tests := []struct {
		name    string
		data    string
		edit    func(map[string]any) error
		wantErr error
	}{
		{name: "array payload", data: `[1]`, edit: keep},
		{name: "null payload", data: `null`, edit: keep},
		{name: "invalid JSON", data: `{`, edit: keep},
		{name: "failing edit", data: `{}`, edit: func(map[string]any) error { return errEdit }, wantErr: errEdit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := eventstore.UpcastObject(tt.edit)([]byte(tt.data))
			if err == nil {
				t.Fatal("UpcastObject returned no error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
