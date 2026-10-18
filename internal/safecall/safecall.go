// Package safecall runs user callbacks (handlers, reactions, jobs) under a
// timeout and turns their panics into errors, the same way for every
// runner.
package safecall

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"
)

// ErrTimeout marks a callback that returned nil after its timeout passed,
// when Input.LateSuccessFails is set. The error also matches
// context.DeadlineExceeded.
var ErrTimeout = errors.New("callback exceeded its timeout")

// PanicError is a panic recovered from a callback.
type PanicError struct {
	// Value is what the callback panicked with.
	Value any
	// Stack is the stack of the panicking goroutine.
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// Input configures Call.
type Input struct {
	// Timeout bounds the callback's ctx; zero leaves ctx as it is.
	Timeout time.Duration
	// LateSuccessFails turns a nil result returned after Timeout passed
	// into an ErrTimeout error, for callers whose side effects may already
	// have been cut short.
	LateSuccessFails bool
}

// Call runs fn with ctx bounded by in.Timeout. A panic in fn is returned as
// *PanicError.
func Call(ctx context.Context, in Input, fn func(ctx context.Context) error) (err error) {
	if in.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, in.Timeout)
		defer cancel()
	}
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	err = fn(ctx)
	if err == nil && in.LateSuccessFails && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w %s: %w", ErrTimeout, in.Timeout, ctx.Err())
	}
	return err
}
