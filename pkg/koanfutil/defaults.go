package koanfutil

import (
	"fmt"
	"reflect"

	"github.com/knadh/koanf/v2"
)

// WithDefaults returns a koanf.Provider that provides default values from a struct.
// The struct fields must have `koanf` tags to define the key names.
//
// Usage:
//
//	k := koanf.New(".")
//	k.Load(koanfutil.WithDefaults(postgres.DefaultConfig()), nil)
//	k.Load(file.Provider("config.toml"), toml.Parser())
func WithDefaults[T any](defaults T) koanf.Provider {
	return &defaultsProvider[T]{defaults: defaults}
}

type defaultsProvider[T any] struct {
	defaults T
}

// Read converts the defaults struct to a map using koanf tags.
func (p *defaultsProvider[T]) Read() (map[string]any, error) {
	return structToMap(p.defaults)
}

// ReadBytes is not supported for this provider.
func (p *defaultsProvider[T]) ReadBytes() ([]byte, error) {
	return nil, fmt.Errorf("koanfutil: ReadBytes not supported")
}

// structToMap converts a struct to map[string]any using koanf tags.
func structToMap(v any) (map[string]any, error) {
	val := reflect.ValueOf(v)
	if val.Kind() == reflect.Ptr {
		if val.IsNil() {
			return nil, nil
		}
		val = val.Elem()
	}

	if val.Kind() != reflect.Struct {
		return nil, fmt.Errorf("koanfutil: expected struct, got %T", v)
	}

	result := make(map[string]any)
	typ := val.Type()

	for i := range typ.NumField() {
		field := typ.Field(i)
		fieldVal := val.Field(i)

		if !field.IsExported() {
			continue
		}

		key := field.Tag.Get("koanf")
		if key == "" || key == "-" {
			continue
		}

		if isZeroValue(fieldVal) {
			continue
		}

		if fieldVal.Kind() == reflect.Ptr {
			if fieldVal.IsNil() {
				continue
			}
			fieldVal = fieldVal.Elem()
		}

		if fieldVal.Kind() == reflect.Struct && !isSpecialType(fieldVal) {
			nested, err := structToMap(fieldVal.Interface())
			if err != nil {
				return nil, err
			}
			if len(nested) > 0 {
				result[key] = nested
			}
		} else {
			result[key] = fieldVal.Interface()
		}
	}

	return result, nil
}

// isZeroValue reports whether v is the zero value for its type.
func isZeroValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		return v.IsNil()
	case reflect.Slice, reflect.Map:
		return v.IsNil() || v.Len() == 0
	default:
		return v.IsZero()
	}
}

// isSpecialType returns true for types that should be treated as values, not nested structs.
func isSpecialType(v reflect.Value) bool {
	t := v.Type()
	// time.Duration and time.Time are structs but should be treated as values
	return t.PkgPath() == "time"
}
