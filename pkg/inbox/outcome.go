package inbox

import "errors"

// Ignore returns the Handler result that settles a message as ignored, for
// a message that is valid but needs no action. The reason is stored as the
// message's error.
func Ignore(reason string) error { return &ignoreError{reason: reason} }

// Permanent returns the Handler result that Parks a message immediately,
// without further retries. A nil err is replaced by a generic one.
func Permanent(err error) error {
	if err == nil {
		err = errors.New("permanent failure")
	}
	return &permanentError{err: err}
}

type ignoreError struct{ reason string }

func (e *ignoreError) Error() string { return "inbox: ignored: " + e.reason }

type permanentError struct{ err error }

func (e *permanentError) Error() string { return "inbox: permanent: " + e.err.Error() }

func (e *permanentError) Unwrap() error { return e.err }
