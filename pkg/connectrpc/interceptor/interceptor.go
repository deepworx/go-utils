// Package interceptor provides default interceptor chain builders for Connect RPC services.
package interceptor

import (
	"fmt"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"connectrpc.com/validate"

	"github.com/deepworx/go-utils/pkg/connectrpc/deadline"
	"github.com/deepworx/go-utils/pkg/connectrpc/errors"
	"github.com/deepworx/go-utils/pkg/connectrpc/jwtauth"
	"github.com/deepworx/go-utils/pkg/connectrpc/logging"
	"github.com/deepworx/go-utils/pkg/connectrpc/recovery"
	"github.com/deepworx/go-utils/pkg/connectrpc/requestid"
)

// Options configures the interceptor chain.
type Options struct {
	deadlineCfg  *deadline.Config
	requestIDCfg *requestid.Config
}

// Option configures the interceptor builder.
type Option func(*Options)

// WithDeadline overrides the default deadline configuration.
func WithDeadline(cfg deadline.Config) Option {
	return func(o *Options) {
		o.deadlineCfg = &cfg
	}
}

// WithRequestID overrides the default request ID configuration.
func WithRequestID(cfg requestid.Config) Option {
	return func(o *Options) {
		o.requestIDCfg = &cfg
	}
}

// BuildDefault creates a standard interceptor chain without authentication.
// Returns interceptors in order: recovery, deadline, requestid, otel, logging, validate, errors.
func BuildDefault(opts ...Option) ([]connect.Interceptor, error) {
	o := &Options{}
	for _, opt := range opts {
		opt(o)
	}
	return buildChain(o, nil)
}

// BuildDefaultWithAuth creates a standard interceptor chain with JWT authentication.
// Returns interceptors in order: recovery, deadline, requestid, otel, logging, jwtauth, validate, errors.
// Returns error if auth is nil.
func BuildDefaultWithAuth(auth *jwtauth.Authenticator, opts ...Option) ([]connect.Interceptor, error) {
	if auth == nil {
		return nil, fmt.Errorf("build interceptors: authenticator is required")
	}
	o := &Options{}
	for _, opt := range opts {
		opt(o)
	}
	return buildChain(o, auth)
}

func buildChain(o *Options, auth *jwtauth.Authenticator) ([]connect.Interceptor, error) {
	interceptors := make([]connect.Interceptor, 0, 8)

	// 1. Recovery - always first, catches panics from all downstream
	interceptors = append(interceptors, recovery.NewInterceptor())

	// 2. Deadline - enforces timeouts early
	deadlineCfg := deadline.DefaultConfig()
	if o.deadlineCfg != nil {
		deadlineCfg = *o.deadlineCfg
	}
	interceptors = append(interceptors, deadline.NewInterceptor(deadlineCfg))

	// 3. RequestID - generates ID before logging/tracing uses it
	requestIDCfg := requestid.DefaultConfig()
	if o.requestIDCfg != nil {
		requestIDCfg = *o.requestIDCfg
	}
	interceptors = append(interceptors, requestid.NewInterceptor(requestIDCfg))

	// 4. OTel - captures full span including auth/validation time
	otelInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		return nil, fmt.Errorf("create otel interceptor: %w", err)
	}
	interceptors = append(interceptors, otelInterceptor)

	// 5. Logging - logs with request ID context
	interceptors = append(interceptors, logging.NewInterceptor())

	// 6. Auth (optional) - validates JWT after observability setup
	if auth != nil {
		interceptors = append(interceptors, jwtauth.NewInterceptor(auth))
	}

	// 7. Validate - validates request payloads after auth
	interceptors = append(interceptors, validate.NewInterceptor())

	// 8. Errors - always last, maps all errors to Connect codes
	interceptors = append(interceptors, errors.NewInterceptor())

	return interceptors, nil
}
