// Package requestid provides request ID propagation for Connect RPC handlers.
package requestid

import (
	"context"
	"encoding/hex"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/safeid"
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

// NewInterceptor creates a Connect RPC server interceptor that propagates or
// generates request IDs. It extracts the request ID from the configured header,
// or generates a new UUID v4 if the header is missing or is not 1-128 bytes of
// ASCII letters, digits or ".", "_", ":", "-". The request ID is stored in the
// context via ctxutil.WithRequestID.
func NewInterceptor(cfg Config) connect.ServerInterceptor {
	headerName := cfg.HeaderName
	if headerName == "" {
		headerName = DefaultConfig().HeaderName
	}
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			return next(ensureRequestID(ctx, headerName), spec, stream)
		}
	}
}

func ensureRequestID(ctx context.Context, headerName string) context.Context {
	var id string
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		id = info.RequestHeader().Get(headerName)
	}
	if !safeid.Valid(id) {
		id = generateID()
	}
	return ctxutil.WithRequestID(ctx, id)
}

func generateID() string {
	id := uuid.New()
	return hex.EncodeToString(id[:])
}
