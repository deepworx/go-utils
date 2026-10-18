package eventstore

import (
	"encoding"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/deepworx/go-utils/pkg/eventsourcing/aggregate"
)

// maxSampleDepth bounds how deep CheckCodec fills recursive types.
const maxSampleDepth = 6

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
	rawMessageType    = reflect.TypeFor[json.RawMessage]()
)

// CheckCodec fails t for every event registered on codec that has an
// exported field without a `json` tag, directly or in a nested type, or that
// does not survive an Encode and Decode round trip with every settable field
// filled. Call it from a test of the service that registers its events.
func CheckCodec(t testing.TB, codec *Codec) {
	t.Helper()

	for _, eventType := range codec.eventTypes() {
		reg, err := codec.registration(eventType)
		if err != nil {
			t.Errorf("CheckCodec %q: %v", eventType, err)
			continue
		}
		for _, path := range untaggedFields(reg.typ) {
			t.Errorf("CheckCodec %q: exported field %s has no json tag", eventType, path)
		}
		codec.checkRoundTrip(t, roundTrip{eventType: eventType, typ: reg.typ})
	}
}

// roundTrip is a registered event type and its Go type.
type roundTrip struct {
	eventType string
	typ       reflect.Type
}

func (c *Codec) checkRoundTrip(t testing.TB, in roundTrip) {
	t.Helper()

	codec, eventType, typ := c, in.eventType, in.typ
	sample := reflect.New(typ).Elem()
	fillSample(sample, 0)
	event, ok := sample.Interface().(aggregate.Event)
	if !ok {
		t.Errorf("CheckCodec %q: %s does not implement aggregate.Event", eventType, typ)
		return
	}
	payload, err := codec.Encode(event)
	if err != nil {
		t.Errorf("CheckCodec %q: %v", eventType, err)
		return
	}
	decoded, err := codec.Decode(payload)
	if err != nil {
		t.Errorf("CheckCodec %q: %v", eventType, err)
		return
	}
	if !reflect.DeepEqual(decoded, event) {
		t.Errorf("CheckCodec %q: round trip changed the event:\n got %#v\nwant %#v\njson %s", eventType, decoded, event, payload.Data)
	}
}

func (c *Codec) eventTypes() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	types := make([]string, 0, len(c.types))
	for eventType := range c.types {
		types = append(types, eventType)
	}
	slices.Sort(types)
	return types
}

// untaggedFields returns the paths of exported struct fields reachable from
// typ that encoding/json would serialize without a json tag. Types with their
// own JSON or text encoding are not inspected.
func untaggedFields(typ reflect.Type) []string {
	var paths []string
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type, path string)
	walk = func(t reflect.Type, path string) {
		if seen[t] || implementsMarshaler(t) {
			return
		}
		seen[t] = true
		switch t.Kind() {
		case reflect.Pointer:
			walk(t.Elem(), path)
		case reflect.Slice, reflect.Array, reflect.Map:
			walk(t.Elem(), path+"[]")
		case reflect.Struct:
			for i := range t.NumField() {
				f := t.Field(i)
				fieldPath := joinPath(path, f.Name)
				_, tagged := f.Tag.Lookup("json")
				switch {
				case tagged && f.Tag.Get("json") == "-":
				case f.Anonymous && !tagged && isStruct(f.Type):
					walk(f.Type, path)
				case !f.IsExported():
				case !tagged:
					paths = append(paths, fieldPath)
				default:
					walk(f.Type, fieldPath)
				}
			}
		default:
		}
	}
	walk(typ, "")
	return paths
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}

func isStruct(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

func implementsMarshaler(t reflect.Type) bool {
	pt := reflect.PointerTo(t)
	return t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType) ||
		pt.Implements(jsonMarshalerType) || pt.Implements(textMarshalerType)
}

// fillSample sets every settable value reachable from v to a non-zero
// sample, so a round trip exercises each field. Fields tagged `json:"-"`
// stay zero because encoding/json drops them.
func fillSample(v reflect.Value, depth int) {
	if depth > maxSampleDepth || !v.CanSet() {
		return
	}
	if v.Type() == rawMessageType {
		v.SetBytes([]byte(`{}`))
		return
	}
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.String:
		v.SetString("sample")
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillSample(p.Elem(), depth+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillSample(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Array:
		for i := range v.Len() {
			fillSample(v.Index(i), depth+1)
		}
	case reflect.Map:
		m := reflect.MakeMapWithSize(v.Type(), 1)
		key := reflect.New(v.Type().Key()).Elem()
		elem := reflect.New(v.Type().Elem()).Elem()
		fillSample(key, depth+1)
		fillSample(elem, depth+1)
		m.SetMapIndex(key, elem)
		v.Set(m)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).Tag.Get("json") == "-" {
				continue
			}
			fillSample(v.Field(i), depth+1)
		}
	default:
	}
}
