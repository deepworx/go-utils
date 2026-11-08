// Package errors provides error mapping for Connect RPC handlers.
package errors

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"
)

// ConnectCoder allows errors to specify their Connect RPC error code.
// It is still honored, after the rules passed with WithRules.
//
// Deprecated: Map errors with WithRules at the service edge instead, so
// domain and app errors need not import connectrpc.com/connect.
type ConnectCoder interface {
	ConnectCode() connect.Code
}

type options struct {
	rules  []Rule
	domain string
}

// Option configures the error mapping of NewInterceptor and Map.
type Option func(*options)

// WithRules appends rules to the mapping. Rules are tried in order and the
// first match wins.
func WithRules(rules ...Rule) Option {
	return func(o *options) {
		o.rules = append(o.rules, rules...)
	}
}

// WithDomain sets the Domain of the google.rpc.ErrorInfo details that rules
// with a reason attach, typically the service's DNS name. Without it the
// Domain is empty. The last WithDomain wins.
func WithDomain(domain string) Option {
	return func(o *options) {
		o.domain = domain
	}
}

func newOptions(opts []Option) *options {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// NewInterceptor creates a Connect RPC server interceptor that maps errors to
// appropriate Connect codes.
//
// Error mapping priority:
//  1. context.Canceled → CodeCanceled
//  2. context.DeadlineExceeded → CodeDeadlineExceeded
//  3. Rules passed with WithRules, in order → code of the first match
//  4. ConnectCoder interface → code from ConnectCode()
//  5. *connect.Error → preserved as-is
//  6. Any other error → CodeInternal with message "internal error"
//
// For mapped errors (1-4), the original message is sent to the client.
// For unmapped errors (6), the message is sanitized to hide internal details.
// The original error stays available to errors.Is and errors.As as the cause.
// Without WithRules, step 3 is skipped.
func NewInterceptor(opts ...Option) connect.ServerInterceptor {
	o := newOptions(opts)
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			if err := next(ctx, spec, stream); err != nil {
				return o.mapError(err)
			}
			return nil
		}
	}
}

// Map maps err to a *connect.Error the way NewInterceptor does. A nil err
// returns nil.
func Map(err error, opts ...Option) *connect.Error {
	if err == nil {
		return nil
	}
	return newOptions(opts).mapError(err)
}

func (o *options) mapError(err error) *connect.Error {
	if errors.Is(err, context.Canceled) {
		return withMessage(connect.CodeCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return withMessage(connect.CodeDeadlineExceeded, err)
	}

	for _, rule := range o.rules {
		if rule.matches(err) {
			return rule.apply(err, o.domain)
		}
	}

	var coder ConnectCoder
	if errors.As(err, &coder) {
		return withMessage(coder.ConnectCode(), err)
	}

	if connectErr, ok := errors.AsType[*connect.Error](err); ok {
		return connectErr
	}

	return connect.NewError(connect.CodeInternal, "internal error").WithCause(err)
}

// withMessage maps err to code, sending its message and keeping it as the
// cause.
func withMessage(code connect.Code, err error) *connect.Error {
	return connect.NewError(code, err.Error()).WithCause(err)
}
