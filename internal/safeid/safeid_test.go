package safeid_test

import (
	"strings"
	"testing"

	"github.com/deepworx/go-utils/internal/safeid"
)

func TestValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "letters and digits", id: "Req42", want: true},
		{name: "allowed punctuation", id: "a.b_c:d-e", want: true},
		{name: "max length", id: strings.Repeat("a", safeid.MaxLen), want: true},
		{name: "empty", id: ""},
		{name: "too long", id: strings.Repeat("a", safeid.MaxLen+1)},
		{name: "space", id: "a b"},
		{name: "slash", id: "a/b"},
		{name: "newline", id: "a\nb"},
		{name: "NUL", id: "a\x00b"},
		{name: "non-ASCII", id: "räq"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := safeid.Valid(tt.id); got != tt.want {
				t.Errorf("Valid(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
