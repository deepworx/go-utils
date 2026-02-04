package nats

import (
	"testing"
)

func TestMeterName(t *testing.T) {
	t.Parallel()

	expected := "github.com/deepworx/go-utils/pkg/nats"
	if meterName != expected {
		t.Errorf("meterName = %q, want %q", meterName, expected)
	}
}
