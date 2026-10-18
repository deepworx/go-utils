// Package safeid checks identifiers taken from untrusted input, such as a
// client's request ID, before they are logged or stored.
package safeid

// MaxLen is the longest identifier Valid accepts, in bytes.
const MaxLen = 128

// Valid reports whether id is 1 to MaxLen bytes of ASCII letters, digits
// or ".", "_", ":", "-".
func Valid(id string) bool {
	if id == "" || len(id) > MaxLen {
		return false
	}
	for i := range len(id) {
		switch c := id[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}
