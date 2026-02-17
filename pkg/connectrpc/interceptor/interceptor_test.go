package interceptor

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/deepworx/go-utils/pkg/connectrpc/deadline"
	"github.com/deepworx/go-utils/pkg/connectrpc/jwtauth"
	"github.com/deepworx/go-utils/pkg/connectrpc/requestid"
	"github.com/deepworx/go-utils/pkg/ctxutil"
)

// ---------------------------------------------------------------------------
// Mock types
// ---------------------------------------------------------------------------

type mockRequest struct {
	connect.AnyRequest
	procedure string
	headers   http.Header
	isClient  bool
}

func (r *mockRequest) Any() any            { return nil }
func (r *mockRequest) Spec() connect.Spec  { return connect.Spec{Procedure: r.procedure, IsClient: r.isClient} }
func (r *mockRequest) Header() http.Header { return r.headers }
func (r *mockRequest) Peer() connect.Peer  { return connect.Peer{} }

type mockResponse struct {
	connect.AnyResponse
}

func (r *mockResponse) Any() any             { return nil }
func (r *mockResponse) Header() http.Header  { return http.Header{} }
func (r *mockResponse) Trailer() http.Header { return http.Header{} }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// interceptorPkgName extracts the short package name from a connect.Interceptor
// using reflection (e.g. "*recovery.interceptor" → "recovery").
func interceptorPkgName(ic connect.Interceptor) string {
	t := reflect.TypeOf(ic)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	parts := strings.Split(t.PkgPath(), "/")
	return parts[len(parts)-1]
}

// buildUnaryChain mimics how connect-go applies interceptors:
// the first interceptor in the slice is the outermost wrapper.
func buildUnaryChain(interceptors []connect.Interceptor, handler connect.UnaryFunc) connect.UnaryFunc {
	for i := len(interceptors) - 1; i >= 0; i-- {
		handler = interceptors[i].WrapUnary(handler)
	}
	return handler
}

// ---------------------------------------------------------------------------
// A. Options white-box tests
// ---------------------------------------------------------------------------

func TestWithDeadline_AppliesConfig(t *testing.T) {
	t.Parallel()

	cfg := deadline.Config{
		DefaultTimeout: 45 * time.Second,
		MaxTimeout:     180 * time.Second,
	}

	o := &Options{}
	WithDeadline(cfg)(o)

	if o.deadlineCfg == nil {
		t.Fatal("deadlineCfg should not be nil after applying WithDeadline")
	}
	if o.deadlineCfg.DefaultTimeout != 45*time.Second {
		t.Errorf("DefaultTimeout = %v, want %v", o.deadlineCfg.DefaultTimeout, 45*time.Second)
	}
	if o.deadlineCfg.MaxTimeout != 180*time.Second {
		t.Errorf("MaxTimeout = %v, want %v", o.deadlineCfg.MaxTimeout, 180*time.Second)
	}
}

func TestWithRequestID_AppliesConfig(t *testing.T) {
	t.Parallel()

	cfg := requestid.Config{HeaderName: "X-Correlation-ID"}

	o := &Options{}
	WithRequestID(cfg)(o)

	if o.requestIDCfg == nil {
		t.Fatal("requestIDCfg should not be nil after applying WithRequestID")
	}
	if o.requestIDCfg.HeaderName != "X-Correlation-ID" {
		t.Errorf("HeaderName = %q, want %q", o.requestIDCfg.HeaderName, "X-Correlation-ID")
	}
}

func TestOptions_IndependentApplication(t *testing.T) {
	t.Parallel()

	o := &Options{}
	WithDeadline(deadline.Config{DefaultTimeout: 10 * time.Second, MaxTimeout: 60 * time.Second})(o)

	if o.requestIDCfg != nil {
		t.Error("requestIDCfg should be nil when only WithDeadline is applied")
	}

	WithRequestID(requestid.Config{HeaderName: "X-Custom"})(o)

	if o.deadlineCfg == nil {
		t.Error("deadlineCfg should still be set after applying WithRequestID")
	}
	if o.requestIDCfg == nil {
		t.Error("requestIDCfg should not be nil after applying WithRequestID")
	}
}

func TestOptions_LastAppliedWins(t *testing.T) {
	t.Parallel()

	o := &Options{}
	WithDeadline(deadline.Config{DefaultTimeout: 10 * time.Second, MaxTimeout: 60 * time.Second})(o)
	WithDeadline(deadline.Config{DefaultTimeout: 45 * time.Second, MaxTimeout: 180 * time.Second})(o)

	if o.deadlineCfg.DefaultTimeout != 45*time.Second {
		t.Errorf("DefaultTimeout = %v, want %v (last option should win)", o.deadlineCfg.DefaultTimeout, 45*time.Second)
	}
	if o.deadlineCfg.MaxTimeout != 180*time.Second {
		t.Errorf("MaxTimeout = %v, want %v (last option should win)", o.deadlineCfg.MaxTimeout, 180*time.Second)
	}
}

// ---------------------------------------------------------------------------
// B. Builder tests (table-driven, black-box)
// ---------------------------------------------------------------------------

func TestBuildDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		opts      []Option
		wantCount int
	}{
		{
			name:      "no options",
			opts:      nil,
			wantCount: 7,
		},
		{
			name: "with custom deadline",
			opts: []Option{
				WithDeadline(deadline.Config{
					DefaultTimeout: 60_000_000_000,
					MaxTimeout:     300_000_000_000,
				}),
			},
			wantCount: 7,
		},
		{
			name: "with custom requestID",
			opts: []Option{
				WithRequestID(requestid.Config{
					HeaderName: "X-Custom-Request-ID",
				}),
			},
			wantCount: 7,
		},
		{
			name: "with all options",
			opts: []Option{
				WithDeadline(deadline.Config{
					DefaultTimeout: 60_000_000_000,
					MaxTimeout:     300_000_000_000,
				}),
				WithRequestID(requestid.Config{
					HeaderName: "X-Custom-Request-ID",
				}),
			},
			wantCount: 7,
		},
		{
			name:      "empty options slice",
			opts:      []Option{},
			wantCount: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			interceptors, err := BuildDefault(tt.opts...)
			if err != nil {
				t.Fatalf("BuildDefault() error = %v", err)
			}
			if len(interceptors) != tt.wantCount {
				t.Errorf("BuildDefault() returned %d interceptors, want %d", len(interceptors), tt.wantCount)
			}
		})
	}
}

func TestBuildDefaultWithAuth_NilAuth(t *testing.T) {
	t.Parallel()

	_, err := BuildDefaultWithAuth(nil)
	if err == nil {
		t.Fatal("BuildDefaultWithAuth(nil) should return error")
	}
	if !strings.Contains(err.Error(), "authenticator is required") {
		t.Errorf("error = %q, want message containing %q", err.Error(), "authenticator is required")
	}
}

func TestBuildDefaultWithAuth_ValidAuth(t *testing.T) {
	t.Parallel()

	auth := &jwtauth.Authenticator{}

	interceptors, err := BuildDefaultWithAuth(auth)
	if err != nil {
		t.Fatalf("BuildDefaultWithAuth() error = %v", err)
	}
	if len(interceptors) != 8 {
		t.Errorf("BuildDefaultWithAuth() returned %d interceptors, want 8", len(interceptors))
	}
}

func TestBuildDefaultWithAuth_WithOptions(t *testing.T) {
	t.Parallel()

	auth := &jwtauth.Authenticator{}

	interceptors, err := BuildDefaultWithAuth(auth,
		WithDeadline(deadline.Config{
			DefaultTimeout: 60_000_000_000,
			MaxTimeout:     300_000_000_000,
		}),
		WithRequestID(requestid.Config{
			HeaderName: "X-Custom-Request-ID",
		}),
	)
	if err != nil {
		t.Fatalf("BuildDefaultWithAuth() error = %v", err)
	}
	if len(interceptors) != 8 {
		t.Errorf("BuildDefaultWithAuth() returned %d interceptors, want 8", len(interceptors))
	}
}

// ---------------------------------------------------------------------------
// C. Ordering tests (reflect-based)
// ---------------------------------------------------------------------------

func TestBuildDefault_InterceptorOrder(t *testing.T) {
	t.Parallel()

	interceptors, err := BuildDefault()
	if err != nil {
		t.Fatalf("BuildDefault() error = %v", err)
	}

	expectedOrder := []string{
		"recovery",
		"deadline",
		"requestid",
		"otelconnect",
		"logging",
		"validate",
		"errors",
	}

	if len(interceptors) != len(expectedOrder) {
		t.Fatalf("got %d interceptors, want %d", len(interceptors), len(expectedOrder))
	}

	for i, ic := range interceptors {
		pkg := interceptorPkgName(ic)
		if pkg != expectedOrder[i] {
			t.Errorf("interceptor[%d] package = %q, want %q", i, pkg, expectedOrder[i])
		}
	}
}

func TestBuildDefaultWithAuth_InterceptorOrder(t *testing.T) {
	t.Parallel()

	auth := &jwtauth.Authenticator{}
	interceptors, err := BuildDefaultWithAuth(auth)
	if err != nil {
		t.Fatalf("BuildDefaultWithAuth() error = %v", err)
	}

	expectedOrder := []string{
		"recovery",
		"deadline",
		"requestid",
		"otelconnect",
		"logging",
		"jwtauth",
		"validate",
		"errors",
	}

	if len(interceptors) != len(expectedOrder) {
		t.Fatalf("got %d interceptors, want %d", len(interceptors), len(expectedOrder))
	}

	for i, ic := range interceptors {
		pkg := interceptorPkgName(ic)
		if pkg != expectedOrder[i] {
			t.Errorf("interceptor[%d] package = %q, want %q", i, pkg, expectedOrder[i])
		}
	}
}

// ---------------------------------------------------------------------------
// D. Behavioral integration tests
// ---------------------------------------------------------------------------

func TestChain_RecoveryCatchesPanics(t *testing.T) {
	t.Parallel()

	interceptors, err := BuildDefault()
	if err != nil {
		t.Fatalf("BuildDefault() error = %v", err)
	}

	chain := buildUnaryChain(interceptors, func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		panic("handler panic")
	})

	req := &mockRequest{
		procedure: "/test.Service/Method",
		headers:   http.Header{},
	}

	_, err = chain(context.Background(), req)

	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("expected connect.Error, got %T: %v", err, err)
	}
	if connectErr.Code() != connect.CodeInternal {
		t.Errorf("code = %v, want CodeInternal (recovery should catch panics)", connectErr.Code())
	}
}

func TestChain_RequestIDPropagation(t *testing.T) {
	t.Parallel()

	interceptors, err := BuildDefault()
	if err != nil {
		t.Fatalf("BuildDefault() error = %v", err)
	}

	var capturedID string
	chain := buildUnaryChain(interceptors, func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		if id, ok := ctxutil.RequestID(ctx); ok {
			capturedID = id
		}
		return &mockResponse{}, nil
	})

	headers := http.Header{}
	headers.Set("X-Request-ID", "test-correlation-123")
	req := &mockRequest{
		procedure: "/test.Service/Method",
		headers:   headers,
	}

	_, err = chain(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedID != "test-correlation-123" {
		t.Errorf("capturedID = %q, want %q", capturedID, "test-correlation-123")
	}
}

func TestChain_RequestIDGeneration(t *testing.T) {
	t.Parallel()

	interceptors, err := BuildDefault()
	if err != nil {
		t.Fatalf("BuildDefault() error = %v", err)
	}

	var capturedID string
	chain := buildUnaryChain(interceptors, func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		if id, ok := ctxutil.RequestID(ctx); ok {
			capturedID = id
		}
		return &mockResponse{}, nil
	})

	req := &mockRequest{
		procedure: "/test.Service/Method",
		headers:   http.Header{},
	}

	_, err = chain(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedID == "" {
		t.Fatal("expected request ID to be generated when no header is present")
	}
	if len(capturedID) != 32 {
		t.Errorf("generated request ID length = %d, want 32 (UUID hex)", len(capturedID))
	}
}

func TestChain_DeadlineApplied(t *testing.T) {
	t.Parallel()

	interceptors, err := BuildDefault()
	if err != nil {
		t.Fatalf("BuildDefault() error = %v", err)
	}

	var hasDeadline bool
	chain := buildUnaryChain(interceptors, func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		_, hasDeadline = ctx.Deadline()
		return &mockResponse{}, nil
	})

	req := &mockRequest{
		procedure: "/test.Service/Method",
		headers:   http.Header{},
	}

	// Use a context without deadline to verify the interceptor adds one.
	_, err = chain(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !hasDeadline {
		t.Error("expected context to have a deadline after passing through the chain")
	}
}

func TestChain_ErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		handlerErr  error
		wantCode    connect.Code
		wantMessage string
	}{
		{
			name:        "raw error mapped to CodeInternal",
			handlerErr:  errors.New("database connection failed"),
			wantCode:    connect.CodeInternal,
			wantMessage: "internal error",
		},
		{
			name:        "context.Canceled mapped to CodeCanceled",
			handlerErr:  context.Canceled,
			wantCode:    connect.CodeCanceled,
			wantMessage: "context canceled",
		},
		{
			name:        "context.DeadlineExceeded mapped to CodeDeadlineExceeded",
			handlerErr:  context.DeadlineExceeded,
			wantCode:    connect.CodeDeadlineExceeded,
			wantMessage: "context deadline exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			interceptors, err := BuildDefault()
			if err != nil {
				t.Fatalf("BuildDefault() error = %v", err)
			}

			chain := buildUnaryChain(interceptors, func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
				return nil, tt.handlerErr
			})

			req := &mockRequest{
				procedure: "/test.Service/Method",
				headers:   http.Header{},
			}

			_, err = chain(context.Background(), req)

			var connectErr *connect.Error
			if !errors.As(err, &connectErr) {
				t.Fatalf("expected connect.Error, got %T: %v", err, err)
			}
			if connectErr.Code() != tt.wantCode {
				t.Errorf("code = %v, want %v", connectErr.Code(), tt.wantCode)
			}
			if connectErr.Message() != tt.wantMessage {
				t.Errorf("message = %q, want %q", connectErr.Message(), tt.wantMessage)
			}
		})
	}
}

func TestChain_SuccessPath(t *testing.T) {
	t.Parallel()

	interceptors, err := BuildDefault()
	if err != nil {
		t.Fatalf("BuildDefault() error = %v", err)
	}

	handlerCalled := false
	chain := buildUnaryChain(interceptors, func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		handlerCalled = true
		return &mockResponse{}, nil
	})

	req := &mockRequest{
		procedure: "/test.Service/Method",
		headers:   http.Header{},
	}

	resp, err := chain(context.Background(), req)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Error("expected non-nil response")
	}
	if !handlerCalled {
		t.Error("expected handler to be called")
	}
}

// ---------------------------------------------------------------------------
// E. Options integration tests
// ---------------------------------------------------------------------------

func TestChain_CustomDeadlineConfig(t *testing.T) {
	t.Parallel()

	customTimeout := 5 * time.Second
	interceptors, err := BuildDefault(WithDeadline(deadline.Config{
		DefaultTimeout: customTimeout,
		MaxTimeout:     10 * time.Second,
	}))
	if err != nil {
		t.Fatalf("BuildDefault() error = %v", err)
	}

	var contextDeadline time.Time
	var hasDeadline bool
	chain := buildUnaryChain(interceptors, func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		contextDeadline, hasDeadline = ctx.Deadline()
		return &mockResponse{}, nil
	})

	req := &mockRequest{
		procedure: "/test.Service/Method",
		headers:   http.Header{},
	}

	before := time.Now()
	_, err = chain(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !hasDeadline {
		t.Fatal("expected context to have a deadline")
	}

	maxExpected := before.Add(customTimeout + time.Second) // allow 1s tolerance
	if contextDeadline.After(maxExpected) {
		t.Errorf("context deadline = %v, should be within ~%v of %v", contextDeadline, customTimeout, before)
	}
	if contextDeadline.Before(before) {
		t.Errorf("context deadline = %v, should be after %v", contextDeadline, before)
	}
}

func TestChain_CustomRequestIDHeader(t *testing.T) {
	t.Parallel()

	interceptors, err := BuildDefault(WithRequestID(requestid.Config{
		HeaderName: "X-Correlation-ID",
	}))
	if err != nil {
		t.Fatalf("BuildDefault() error = %v", err)
	}

	var capturedID string
	chain := buildUnaryChain(interceptors, func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		if id, ok := ctxutil.RequestID(ctx); ok {
			capturedID = id
		}
		return &mockResponse{}, nil
	})

	headers := http.Header{}
	headers.Set("X-Correlation-ID", "custom-correlation-456")
	req := &mockRequest{
		procedure: "/test.Service/Method",
		headers:   headers,
	}

	_, err = chain(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedID != "custom-correlation-456" {
		t.Errorf("capturedID = %q, want %q", capturedID, "custom-correlation-456")
	}
}

func TestBuildDefault_OptionsOrderIndependence(t *testing.T) {
	t.Parallel()

	deadlineCfg := deadline.Config{DefaultTimeout: 45 * time.Second, MaxTimeout: 180 * time.Second}
	requestIDCfg := requestid.Config{HeaderName: "X-Custom"}

	interceptorsAB, err := BuildDefault(WithDeadline(deadlineCfg), WithRequestID(requestIDCfg))
	if err != nil {
		t.Fatalf("BuildDefault(deadline, requestid) error = %v", err)
	}

	interceptorsBA, err := BuildDefault(WithRequestID(requestIDCfg), WithDeadline(deadlineCfg))
	if err != nil {
		t.Fatalf("BuildDefault(requestid, deadline) error = %v", err)
	}

	if len(interceptorsAB) != len(interceptorsBA) {
		t.Fatalf("option order changed interceptor count: %d vs %d", len(interceptorsAB), len(interceptorsBA))
	}

	for i := range interceptorsAB {
		pkgAB := interceptorPkgName(interceptorsAB[i])
		pkgBA := interceptorPkgName(interceptorsBA[i])
		if pkgAB != pkgBA {
			t.Errorf("interceptor[%d] type differs: %q vs %q", i, pkgAB, pkgBA)
		}
	}
}
