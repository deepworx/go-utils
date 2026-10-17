package notify

import (
	"fmt"
	"strings"
)

// maxChannelBytes is NAMEDATALEN-1; Postgres silently truncates longer
// identifiers, which would make two channels collide.
const maxChannelBytes = 63

// CheckChannel reports whether channel can be used with LISTEN. A valid name
// is non-empty, at most 63 bytes long and contains no NUL byte. Any other
// character is allowed because names are always quoted.
func CheckChannel(channel string) error {
	switch {
	case channel == "":
		return fmt.Errorf("%w: empty name", ErrInvalidChannel)
	case len(channel) > maxChannelBytes:
		return fmt.Errorf("%w: %q is %d bytes, max %d", ErrInvalidChannel, channel, len(channel), maxChannelBytes)
	case strings.ContainsRune(channel, 0):
		return fmt.Errorf("%w: %q contains a NUL byte", ErrInvalidChannel, channel)
	}
	return nil
}
