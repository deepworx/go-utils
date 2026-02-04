package nats

import "errors"

// ErrURLRequired is returned when URL is empty in Config.
var ErrURLRequired = errors.New("url is required")

// ErrIncompleteMTLS is returned when mTLS config has cert_file but no key_file or vice versa.
var ErrIncompleteMTLS = errors.New("mtls requires both cert_file and key_file")
