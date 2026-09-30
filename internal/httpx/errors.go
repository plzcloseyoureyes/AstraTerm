package httpx

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// HTTPError is an error with an HTTP status and a stable machine code, rendered as {"error": msg, "code": code}.
// Two HTTPErrors (or a model.Error) with the same code match under errors.Is. Handlers simply return it.
type HTTPError struct {
	Status  int
	Code    string
	Message string
	// RetryAfter, when > 0, is sent as a Retry-After header (seconds).
	RetryAfter int
	cause      error
}

func (e *HTTPError) Error() string {
	if e.cause != nil {
		return e.Message + ": " + e.cause.Error()
	}
	return e.Message
}

// ErrorCode returns the machine code.
func (e *HTTPError) ErrorCode() string { return e.Code }

// StatusCode returns the HTTP status (echo.HTTPStatusCoder).
func (e *HTTPError) StatusCode() int { return e.Status }

// Is matches errors with the same code.
func (e *HTTPError) Is(target error) bool {
	t, ok := target.(interface{ ErrorCode() string })
	return ok && t.ErrorCode() == e.Code
}

// Unwrap exposes the internal cause (never sent to clients).
func (e *HTTPError) Unwrap() error { return e.cause }

// Typed errors (SPEC §3).
var (
	ErrNotFound        = &HTTPError{Status: http.StatusNotFound, Code: model.CodeNotFound, Message: "not found"}
	ErrForbidden       = &HTTPError{Status: http.StatusForbidden, Code: model.CodeForbidden, Message: "forbidden"}
	ErrUnauthorized    = &HTTPError{Status: http.StatusUnauthorized, Code: model.CodeUnauthorized, Message: "authentication required"}
	ErrLocked          = &HTTPError{Status: http.StatusLocked, Code: model.CodeLocked, Message: "vault is locked"}
	ErrTooManyRequests = &HTTPError{Status: http.StatusTooManyRequests, Code: model.CodeTooManyRequests, Message: "too many requests"}
	ErrInternal        = &HTTPError{Status: http.StatusInternalServerError, Code: model.CodeInternal, Message: "internal server error"}
)

// NewError builds an HTTPError with an arbitrary status/code/message.
func NewError(status int, code, msg string) *HTTPError {
	return &HTTPError{Status: status, Code: code, Message: msg}
}

// BadRequest → 400 {code:"bad_request"}.
func BadRequest(msg string) error { return NewError(http.StatusBadRequest, model.CodeBadRequest, msg) }

// Conflict → 409 {code:"conflict"}.
func Conflict(msg string) error { return NewError(http.StatusConflict, model.CodeConflict, msg) }

// NotFound → 404 with a custom message.
func NotFound(msg string) error { return NewError(http.StatusNotFound, model.CodeNotFound, msg) }

// Forbidden → 403 with a custom message.
func Forbidden(msg string) error { return NewError(http.StatusForbidden, model.CodeForbidden, msg) }

// Unauthorized → 401 with a custom code (e.g. "totp_required") and message.
func Unauthorized(code, msg string) error {
	if code == "" {
		code = model.CodeUnauthorized
	}
	return NewError(http.StatusUnauthorized, code, msg)
}

// TooManyRequests → 429 with a Retry-After hint in seconds.
func TooManyRequests(msg string, retryAfterSec int) error {
	e := NewError(http.StatusTooManyRequests, model.CodeTooManyRequests, msg)
	e.RetryAfter = retryAfterSec
	return e
}

// Internal wraps an unexpected error as a 500 whose details are logged but never sent to the client.
func Internal(cause error) error {
	return &HTTPError{Status: http.StatusInternalServerError, Code: model.CodeInternal, Message: "internal server error", cause: cause}
}

var codeStatus = map[string]int{
	model.CodeNotFound:        http.StatusNotFound,
	model.CodeForbidden:       http.StatusForbidden,
	model.CodeUnauthorized:    http.StatusUnauthorized,
	model.CodeBadRequest:      http.StatusBadRequest,
	model.CodeConflict:        http.StatusConflict,
	model.CodeLocked:          http.StatusLocked,
	model.CodeTooManyRequests: http.StatusTooManyRequests,
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// handleError is the echo.HTTPErrorHandler: it renders err as a JSON error response (see classify), adding
// Retry-After for rate-limit errors. Nothing is written when the response was already committed (streams,
// WebSockets). 5xx details are logged by the request logger, never sent.
func (r *Router) handleError(c *echo.Context, err error) {
	if resp, uerr := echo.UnwrapResponse(c.Response()); uerr == nil && resp.Committed {
		return
	}
	status, code, msg := classify(err)
	var he *HTTPError
	if errors.As(err, &he) && he.RetryAfter > 0 {
		c.Response().Header().Set(echo.HeaderRetryAfter, strconv.Itoa(he.RetryAfter))
	}
	if werr := c.JSON(status, errorBody{Error: msg, Code: code}); werr != nil {
		r.log.Debug("cannot write error response", "err", werr)
	}
}

// classify maps an error to (status, code, message): HTTPError as is (5xx without details); *http.MaxBytesError →
// 413 too_large; errors exposing ErrorCode() (model.Error, store errors) by code; echo errors (no route 404, 405,
// 413, …) by status; context deadline → 504, cancellation → 499; anything else → 500 internal.
func classify(err error) (status int, code, msg string) {
	if he, ok := errors.AsType[*HTTPError](err); ok {
		if he.Status >= 500 {
			return he.Status, he.Code, "internal server error"
		}
		return he.Status, he.Code, he.Message
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return http.StatusRequestEntityTooLarge, "too_large", "request body too large"
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		if st, ok := codeStatus[coded.ErrorCode()]; ok {
			return st, coded.ErrorCode(), err.Error()
		}
	}
	var sc echo.HTTPStatusCoder
	if errors.As(err, &sc) {
		if st := sc.StatusCode(); st >= 400 && st < 600 {
			return echoErrorBody(err, st)
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout", "operation timed out"
	case errors.Is(err, context.Canceled):
		return 499, "canceled", "request canceled"
	}
	return http.StatusInternalServerError, model.CodeInternal, "internal server error"
}

// echoErrorBody renders an error from echo or its middleware (e.g. echo.ErrNotFound, echo.ErrMethodNotAllowed,
// echo.ErrStatusRequestEntityTooLarge, echo.NewHTTPError) in the API's vocabulary.
func echoErrorBody(err error, status int) (int, string, string) {
	if status == http.StatusRequestEntityTooLarge {
		return status, "too_large", "request body too large"
	}
	code := strings.ReplaceAll(strings.ToLower(http.StatusText(status)), " ", "_")
	if code == "" {
		code = "error"
	}
	if status >= 500 {
		return status, code, "internal server error"
	}
	msg := strings.ToLower(http.StatusText(status))
	var ee *echo.HTTPError
	if errors.As(err, &ee) && ee.Message != "" && ee.Message != http.StatusText(status) {
		msg = ee.Message // an explicit echo.NewHTTPError message
	}
	return status, code, msg
}
