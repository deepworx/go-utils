package koanfutil

import (
	"testing"
	"time"

	"github.com/knadh/koanf/v2"
)

type testConfig struct {
	Name     string        `koanf:"name"`
	Port     int           `koanf:"port"`
	Timeout  time.Duration `koanf:"timeout"`
	Optional string        `koanf:"optional"`
	Nested   *nestedConfig `koanf:"nested"`
}

type nestedConfig struct {
	Value string `koanf:"value"`
}

func TestWithDefaults(t *testing.T) {
	t.Parallel()

	defaults := testConfig{
		Name:    "test-service",
		Port:    8080,
		Timeout: 30 * time.Second,
		Nested: &nestedConfig{
			Value: "nested-value",
		},
	}

	k := koanf.New(".")
	if err := k.Load(WithDefaults(defaults), nil); err != nil {
		t.Fatalf("Load defaults: %v", err)
	}

	tests := []struct {
		key  string
		want any
	}{
		{"name", "test-service"},
		{"port", 8080},
		{"timeout", 30 * time.Second},
		{"nested.value", "nested-value"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := k.Get(tt.key)
			if got != tt.want {
				t.Errorf("Get(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}

	if k.Exists("optional") {
		t.Error("zero value 'optional' should not be set")
	}
}

func TestWithDefaults_ZeroValues(t *testing.T) {
	t.Parallel()

	defaults := testConfig{}

	k := koanf.New(".")
	if err := k.Load(WithDefaults(defaults), nil); err != nil {
		t.Fatalf("Load defaults: %v", err)
	}

	if len(k.Keys()) != 0 {
		t.Errorf("expected no keys for zero-value struct, got %v", k.Keys())
	}
}

func TestWithDefaults_NilPointer(t *testing.T) {
	t.Parallel()

	defaults := testConfig{
		Name:   "service",
		Nested: nil,
	}

	k := koanf.New(".")
	if err := k.Load(WithDefaults(defaults), nil); err != nil {
		t.Fatalf("Load defaults: %v", err)
	}

	if k.Exists("nested") {
		t.Error("nil nested struct should not be set")
	}
}
