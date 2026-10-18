// Package connecttest serves single RPCs through a connect.Server so server
// interceptors can be tested without a transport.
package connecttest

import (
	"context"

	"connectrpc.com/connect/v2"
)

// Procedure is the procedure Serve registers and calls.
const Procedure = "/test.v1.TestService/Method"

// Call describes the RPC that Serve dispatches.
type Call struct {
	// StreamType of the registered method; the zero value is unary.
	StreamType connect.StreamType
	// Header holds the request headers the client sent.
	Header map[string]string
	// Handler serves the RPC behind the interceptors.
	Handler connect.ServerFunc
}

// Serve registers call.Handler on a connect.Server with the given
// interceptors and dispatches one RPC to it, as a transport would.
func Serve(ctx context.Context, call Call, interceptors ...connect.ServerInterceptor) error {
	spec := connect.Spec{Procedure: Procedure, StreamType: call.StreamType}
	server := connect.NewServer(interceptors...)
	server.Register(connect.Method{Spec: spec, Handler: call.Handler})

	info := &connect.CallInfo{Spec: spec}
	for key, value := range call.Header {
		info.RequestHeader().Set(key, value)
	}
	return server.Call(ctx, Procedure, info, nil)
}
