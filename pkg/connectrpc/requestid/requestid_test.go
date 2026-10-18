package requestid

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/deepworx/go-utils/internal/connecttest"
	"github.com/deepworx/go-utils/pkg/ctxutil"
)

// generatedIDLen is the length of a generateID result: a UUID in hex.
const generatedIDLen = 32

func TestInterceptor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cfg        Config
		streamType connect.StreamType
		header     map[string]string
		wantID     string
	}{
		{
			name:   "default header",
			header: map[string]string{"X-Request-ID": "existing-request-id"},
			wantID: "existing-request-id",
		},
		{
			name:   "custom header",
			cfg:    Config{HeaderName: "X-Correlation-ID"},
			header: map[string]string{"X-Correlation-ID": "corr-123"},
			wantID: "corr-123",
		},
		{
			name:   "custom header ignores default header",
			cfg:    Config{HeaderName: "X-Correlation-ID"},
			header: map[string]string{"X-Request-ID": "existing-request-id"},
		},
		{
			name:       "streaming",
			streamType: connect.StreamTypeBidi,
			header:     map[string]string{"X-Request-ID": "stream-req-456"},
			wantID:     "stream-req-456",
		},
		{
			name: "missing header generates ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			id := serveRequestID(t, tt.cfg, connecttest.Call{StreamType: tt.streamType, Header: tt.header})

			if tt.wantID != "" && id != tt.wantID {
				t.Errorf("request ID = %q, want %q", id, tt.wantID)
			}
			if tt.wantID == "" && len(id) != generatedIDLen {
				t.Errorf("request ID = %q, want a generated ID", id)
			}
		})
	}
}

func TestInterceptor_HeaderValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		keep   bool
	}{
		{name: "hex", header: "0a1b2c3d4e5f", keep: true},
		{name: "uuid", header: "0192f5c4-7d3a-7b2e-9f10-3c4d5e6f7a8b", keep: true},
		{name: "allowed punctuation", header: "svc.api:req_42", keep: true},
		{name: "max length", header: strings.Repeat("a", 128), keep: true},
		{name: "too long", header: strings.Repeat("a", 129)},
		{name: "space", header: "req 1"},
		{name: "newline", header: "req\n1"},
		{name: "non-ascii", header: "räq"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			id := serveRequestID(t, Config{}, connecttest.Call{
				Header: map[string]string{"X-Request-ID": tt.header},
			})

			if tt.keep && id != tt.header {
				t.Errorf("request ID = %q, want header %q kept", id, tt.header)
			}
			if !tt.keep && (id == tt.header || len(id) != generatedIDLen) {
				t.Errorf("request ID = %q, want a generated ID replacing %q", id, tt.header)
			}
		})
	}
}

func TestEnsureRequestID_WithoutCallInfo(t *testing.T) {
	t.Parallel()

	id, ok := ctxutil.RequestID(ensureRequestID(context.Background(), "X-Request-ID"))
	if !ok {
		t.Fatal("expected request ID in context")
	}
	if len(id) != generatedIDLen {
		t.Errorf("request ID = %q, want a generated ID", id)
	}
}

func TestGenerateID_Format(t *testing.T) {
	t.Parallel()

	id := generateID()

	if len(id) != generatedIDLen {
		t.Errorf("ID length = %d, want %d", len(id), generatedIDLen)
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("generated ID contains invalid character: %c", c)
		}
	}
	if id == generateID() {
		t.Error("generated IDs should be unique")
	}
}

func serveRequestID(t *testing.T, cfg Config, call connecttest.Call) string {
	t.Helper()

	var id string
	call.Handler = func(ctx context.Context, _ connect.Spec, _ connect.ServerStream) error {
		id, _ = ctxutil.RequestID(ctx)
		return nil
	}
	if err := connecttest.Serve(context.Background(), call, NewInterceptor(cfg)); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	return id
}
