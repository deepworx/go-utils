package errors

import (
	"net/http"

	"connectrpc.com/connect/v2"
)

// statusClientClosedRequest is the non-standard status the Connect protocol
// assigns to CodeCanceled.
const statusClientClosedRequest = 499

// HTTPStatus returns the HTTP status that the Connect protocol assigns to
// code. Unknown codes get 500, like CodeUnknown. With Map it lets a plain
// HTTP handler answer with the same mapping as the interceptor.
func HTTPStatus(code connect.Code) int {
	switch code {
	case connect.CodeCanceled:
		return statusClientClosedRequest
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeOutOfRange:
		return http.StatusBadRequest
	case connect.CodeDeadlineExceeded:
		return http.StatusGatewayTimeout
	case connect.CodeNotFound:
		return http.StatusNotFound
	case connect.CodeAlreadyExists, connect.CodeAborted:
		return http.StatusConflict
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case connect.CodeUnimplemented:
		return http.StatusNotImplemented
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}
