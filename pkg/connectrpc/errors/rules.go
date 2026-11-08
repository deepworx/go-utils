package errors

import (
	"errors"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

// Rule maps the errors it matches to a Connect code. Build Rules with Is
// and As, and pass them to WithRules. The zero Rule matches nothing.
type Rule struct {
	match  func(error) bool
	code   connect.Code
	reason string
}

// Is returns a Rule that maps errors matching target under errors.Is to
// code.
func Is(target error, code connect.Code) Rule {
	return Rule{
		match: func(err error) bool { return errors.Is(err, target) },
		code:  code,
	}
}

// As returns a Rule that maps errors with an error of type T in their chain,
// as found by errors.As, to code.
func As[T error](code connect.Code) Rule {
	return Rule{
		match: func(err error) bool {
			_, ok := errors.AsType[T](err)
			return ok
		},
		code: code,
	}
}

// WithReason returns a copy of r that attaches a google.rpc.ErrorInfo
// detail to the errors it maps. The detail carries reason and the domain
// set with WithDomain. A reason that is not valid UTF-8 cannot be encoded,
// and the error is then mapped without the detail.
func (r Rule) WithReason(reason string) Rule {
	r.reason = reason
	return r
}

func (r Rule) matches(err error) bool {
	return r.match != nil && r.match(err)
}

func (r Rule) apply(err error, domain string) *connect.Error {
	mapped := withMessage(r.code, err)
	if r.reason == "" {
		return mapped
	}
	detail, detailErr := connectproto.NewErrorDetail(&errdetails.ErrorInfo{Reason: r.reason, Domain: domain})
	if detailErr != nil {
		return mapped
	}
	return mapped.WithDetail(detail)
}
