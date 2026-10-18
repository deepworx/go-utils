package aggregate_test

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
)

type orderID uuid.UUID

type order struct {
	aggregate.Base[orderID]
}

func (*order) StreamType() string    { return "order" }
func (*order) Apply(aggregate.Event) {}

var _ aggregate.Root[orderID] = (*order)(nil)

type invoiceID uuid.UUID

type invoice struct {
	aggregate.Base[invoiceID]
}

func (*invoice) StreamType() string    { return "invoice" }
func (*invoice) Apply(aggregate.Event) {}

// impostor declares every method of Root, including one named base, but does
// not embed Base. The unexported base method belongs to this package, so it
// does not match Root's.
type impostor struct{}

func (*impostor) StreamType() string             { return "impostor" }
func (*impostor) Version() int                   { return 0 }
func (*impostor) Apply(aggregate.Event)          {}
func (*impostor) base() *aggregate.Base[orderID] { return nil } //nolint:unused // exists only to show it does not satisfy Root
func (*impostor) ID() orderID                    { return orderID{} }

func TestRootIsSealedToBase(t *testing.T) {
	t.Parallel()

	root := reflect.TypeFor[aggregate.Root[orderID]]()

	tests := []struct {
		name string
		typ  reflect.Type
		want bool
	}{
		{name: "pointer to type embedding Base", typ: reflect.TypeFor[*order](), want: true},
		{name: "value of type embedding Base", typ: reflect.TypeFor[order](), want: false},
		{name: "type declaring all methods without Base", typ: reflect.TypeFor[*impostor](), want: false},
		{name: "type embedding Base with another ID type", typ: reflect.TypeFor[*invoice](), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.typ.Implements(root); got != tt.want {
				t.Errorf("%v implements Root = %v, want %v", tt.typ, got, tt.want)
			}
		})
	}
}
