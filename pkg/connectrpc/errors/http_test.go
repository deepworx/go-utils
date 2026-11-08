package errors

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
)

// TestHTTPStatus pins the table of the Connect protocol specification
// (https://connectrpc.com/docs/protocol#error-codes).
func TestHTTPStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code connect.Code
		want int
	}{
		{connect.CodeCanceled, 499},
		{connect.CodeUnknown, 500},
		{connect.CodeInvalidArgument, 400},
		{connect.CodeDeadlineExceeded, 504},
		{connect.CodeNotFound, 404},
		{connect.CodeAlreadyExists, 409},
		{connect.CodePermissionDenied, 403},
		{connect.CodeResourceExhausted, 429},
		{connect.CodeFailedPrecondition, 400},
		{connect.CodeAborted, 409},
		{connect.CodeOutOfRange, 400},
		{connect.CodeUnimplemented, 501},
		{connect.CodeInternal, 500},
		{connect.CodeUnavailable, 503},
		{connect.CodeDataLoss, 500},
		{connect.CodeUnauthenticated, 401},
		{connect.Code(0), 500},
		{connect.Code(17), 500},
	}

	for _, tt := range tests {
		t.Run(tt.code.String(), func(t *testing.T) {
			t.Parallel()

			if got := HTTPStatus(tt.code); got != tt.want {
				t.Errorf("HTTPStatus(%v) = %d, want %d", tt.code, got, tt.want)
			}
		})
	}
}

// TestHTTPStatusMatchesConnect checks HTTPStatus against the status that
// connect-go's own Connect protocol implementation writes for each code.
func TestHTTPStatusMatchesConnect(t *testing.T) {
	t.Parallel()

	writer := connecthttp.NewErrorWriter()
	for code := connect.CodeCanceled; code <= connect.CodeUnauthenticated; code++ {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/test.v1.TestService/Method", nil)
			if err := writer.Write(rec, req, connect.NewError(code, "")); err != nil {
				t.Fatalf("write error: %v", err)
			}
			if got := HTTPStatus(code); got != rec.Code {
				t.Errorf("HTTPStatus(%v) = %d, connect-go writes %d", code, got, rec.Code)
			}
		})
	}
}
