package slogutil

import "errors"

// ErrInvalidLevel is returned when Level contains an unrecognized value.
var ErrInvalidLevel = errors.New("invalid log level")

// ErrInvalidFormat is returned when Format contains an unrecognized value.
var ErrInvalidFormat = errors.New("invalid log format")
