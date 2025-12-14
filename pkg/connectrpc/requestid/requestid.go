// Package requestid provides request ID propagation for Connect RPC handlers.
package requestid

import (
	"context"
	"encoding/hex"
	"net/http"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/ctxutil"
)

// Config holds configuration for the request ID interceptor.
type Config struct {
	// HeaderName is the HTTP header to read request IDs from.
	HeaderName string `koanf:"header_name"`
}

// DefaultConfig returns a Config with sensible default values.
func DefaultConfig() Config {
	return Config{
		HeaderName: "X-Request-ID",
	}
}

// NewInterceptor creates a Connect RPC interceptor that propagates or generates request IDs.
// It extracts the request ID from the configured header, or generates a new UUID v4 if missing.
// The request ID is stored in the context via ctxutil.WithRequestID.
func NewInterceptor(cfg Config) connect.Interceptor {
	headerName := cfg.HeaderName
	if headerName == "" {
		headerName = "X-Request-ID"
	}
	return &interceptor{headerName: headerName}
}

type interceptor struct {
	headerName string
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		ctx = i.ensureRequestID(ctx, req.Header())
		return next(ctx, req)
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx = i.ensureRequestID(ctx, conn.RequestHeader())
		return next(ctx, conn)
	}
}

func (i *interceptor) ensureRequestID(ctx context.Context, headers http.Header) context.Context {
	id := headers.Get(i.headerName)
	if id == "" {
		id = generateID()
	}
	return ctxutil.WithRequestID(ctx, id)
}

func generateID() string {
	id := uuid.New()
	return hex.EncodeToString(id[:])
}
