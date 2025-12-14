package postgres

import "errors"

// ErrDSNRequired is returned when DSN is empty in Config.
var ErrDSNRequired = errors.New("dsn is required")
