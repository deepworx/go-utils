package requestid

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	"github.com/deepworx/go-utils/pkg/ctxutil"
)

func TestNewInterceptor_DefaultHeader(t *testing.T) {
	t.Parallel()

	i := NewInterceptor(Config{}).(*interceptor)
	if i.headerName != "X-Request-ID" {
		t.Errorf("headerName = %q, want %q", i.headerName, "X-Request-ID")
	}
}

func TestNewInterceptor_CustomHeader(t *testing.T) {
	t.Parallel()

	i := NewInterceptor(Config{HeaderName: "X-Correlation-ID"}).(*interceptor)
	if i.headerName != "X-Correlation-ID" {
		t.Errorf("headerName = %q, want %q", i.headerName, "X-Correlation-ID")
	}
}

func TestEnsureRequestID_FromHeader(t *testing.T) {
	t.Parallel()

	i := &interceptor{headerName: "X-Request-ID"}
	headers := http.Header{}
	headers.Set("X-Request-ID", "existing-request-id")

	ctx := i.ensureRequestID(context.Background(), headers)

	id, ok := ctxutil.RequestID(ctx)
	if !ok {
		t.Fatal("expected request ID in context")
	}
	if id != "existing-request-id" {
		t.Errorf("request ID = %q, want %q", id, "existing-request-id")
	}
}

func TestEnsureRequestID_Generated(t *testing.T) {
	t.Parallel()

	i := &interceptor{headerName: "X-Request-ID"}
	headers := http.Header{}

	ctx := i.ensureRequestID(context.Background(), headers)

	id, ok := ctxutil.RequestID(ctx)
	if !ok {
		t.Fatal("expected request ID in context")
	}
	if len(id) != 32 {
		t.Errorf("generated ID length = %d, want 32", len(id))
	}
	// Verify it's valid hex
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("generated ID contains invalid character: %c", c)
		}
	}
}

func TestGenerateID_Format(t *testing.T) {
	t.Parallel()

	id := generateID()

	if len(id) != 32 {
		t.Errorf("ID length = %d, want 32", len(id))
	}

	// Should not contain hyphens
	for _, c := range id {
		if c == '-' {
			t.Error("ID should not contain hyphens")
		}
	}

	// Generate another to ensure uniqueness
	id2 := generateID()
	if id == id2 {
		t.Error("generated IDs should be unique")
	}
}

func TestInterceptor_WrapUnary(t *testing.T) {
	t.Parallel()

	interceptor := NewInterceptor(Config{})
	var capturedID string

	wrapped := interceptor.WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		id, _ := ctxutil.RequestID(ctx)
		capturedID = id
		return &mockResponse{}, nil
	})

	headers := http.Header{}
	headers.Set("X-Request-ID", "test-req-123")
	req := &mockRequest{procedure: "/test.Service/Method", headers: headers}

	_, err := wrapped(context.Background(), req)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if capturedID != "test-req-123" {
		t.Errorf("capturedID = %q, want %q", capturedID, "test-req-123")
	}
}

func TestInterceptor_WrapUnary_GeneratesID(t *testing.T) {
	t.Parallel()

	interceptor := NewInterceptor(Config{})
	var capturedID string

	wrapped := interceptor.WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		id, _ := ctxutil.RequestID(ctx)
		capturedID = id
		return &mockResponse{}, nil
	})

	req := &mockRequest{procedure: "/test.Service/Method", headers: http.Header{}}

	_, err := wrapped(context.Background(), req)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(capturedID) != 32 {
		t.Errorf("generated ID length = %d, want 32", len(capturedID))
	}
}

func TestInterceptor_WrapStreamingHandler(t *testing.T) {
	t.Parallel()

	interceptor := NewInterceptor(Config{HeaderName: "X-Correlation-ID"})
	var capturedID string

	wrapped := interceptor.WrapStreamingHandler(func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		id, _ := ctxutil.RequestID(ctx)
		capturedID = id
		return nil
	})

	headers := http.Header{}
	headers.Set("X-Correlation-ID", "stream-req-456")
	conn := &mockStreamingConn{procedure: "/test.Service/Stream", headers: headers}

	err := wrapped(context.Background(), conn)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if capturedID != "stream-req-456" {
		t.Errorf("capturedID = %q, want %q", capturedID, "stream-req-456")
	}
}

func TestInterceptor_WrapStreamingClient_PassThrough(t *testing.T) {
	t.Parallel()

	interceptor := NewInterceptor(Config{})
	called := false
	original := func(_ context.Context, _ connect.Spec) connect.StreamingClientConn {
		called = true
		return nil
	}

	wrapped := interceptor.WrapStreamingClient(original)
	wrapped(context.Background(), connect.Spec{})

	if !called {
		t.Error("expected original function to be called")
	}
}

type mockRequest struct {
	connect.AnyRequest
	procedure string
	headers   http.Header
}

func (r *mockRequest) Spec() connect.Spec {
	return connect.Spec{Procedure: r.procedure}
}

func (r *mockRequest) Header() http.Header {
	return r.headers
}

type mockResponse struct {
	connect.AnyResponse
}

type mockStreamingConn struct {
	connect.StreamingHandlerConn
	procedure string
	headers   http.Header
}

func (c *mockStreamingConn) Spec() connect.Spec {
	return connect.Spec{Procedure: c.procedure}
}

func (c *mockStreamingConn) RequestHeader() http.Header {
	return c.headers
}
