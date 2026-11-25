// Package apperr is the error vocabulary shared by a service's core and
// its adapters.
//
// Core code returns these kinds (apperr.NotFound("task", id), ...) and
// never knows about HTTP. The inbound HTTP adapter maps a kind to a
// status code and the standard error body with WriteHTTP, and the
// metrics layer maps it to a status with Status: client-caused kinds are
// "error", everything else is "failure". So "whose fault was this?" is
// decided in one place for every service.
package apperr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/treyamrich/golang-common/metrics"
)

// Kind classifies an error.
type Kind string

const (
	KindInvalid         Kind = "invalid"         // bad input: 400
	KindUnauthenticated Kind = "unauthenticated" // no or bad credentials: 401
	KindForbidden       Kind = "forbidden"       // authenticated, not allowed: 403
	KindNotFound        Kind = "not_found"       // 404
	KindConflict        Kind = "conflict"        // state conflict, duplicate: 409
	KindUnavailable     Kind = "unavailable"     // dependency down, retry later: 503
	KindInternal        Kind = "internal"        // bug or unexpected: 500
)

// Error is a classified error. Message is safe to show to callers; the
// wrapped Err is for logs only and never leaves the process.
type Error struct {
	Kind    Kind
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// New returns an Error of kind with a caller-safe message.
func New(kind Kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// Wrap classifies err as kind, keeping err for logs.
func Wrap(err error, kind Kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...), Err: err}
}

func Invalid(format string, args ...any) *Error   { return New(KindInvalid, format, args...) }
func Forbidden(format string, args ...any) *Error { return New(KindForbidden, format, args...) }
func Conflict(format string, args ...any) *Error  { return New(KindConflict, format, args...) }
func Unauthenticated(format string, args ...any) *Error {
	return New(KindUnauthenticated, format, args...)
}

// NotFound reports that the named thing doesn't exist.
func NotFound(what string, id any) *Error { return New(KindNotFound, "%s %v not found", what, id) }

// Unavailable wraps a dependency failure the caller may retry.
func Unavailable(err error, dependency string) *Error {
	return Wrap(err, KindUnavailable, "%s unavailable", dependency)
}

// KindOf returns err's kind: the outermost *Error's kind, KindInternal
// for any other non-nil error, and "" for nil.
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// Is reports whether err has the given kind.
func Is(err error, kind Kind) bool { return KindOf(err) == kind }

// classified reports whether err carries an explicit (non-internal)
// kind, which then wins over any context error it wraps.
func classified(err error) bool {
	k := KindOf(err)
	return k != "" && k != KindInternal
}

// HTTPStatus maps err to a status code. An unclassified cancelled
// context maps to 499 (client closed request): the caller left, it is
// not our failure. An unclassified deadline is a 504.
func HTTPStatus(err error) int {
	if !classified(err) {
		if errors.Is(err, context.Canceled) {
			return 499
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return http.StatusGatewayTimeout
		}
	}
	switch KindOf(err) {
	case "":
		return http.StatusOK
	case KindInvalid:
		return http.StatusBadRequest
	case KindUnauthenticated:
		return http.StatusUnauthorized
	case KindForbidden:
		return http.StatusForbidden
	case KindNotFound:
		return http.StatusNotFound
	case KindConflict:
		return http.StatusConflict
	case KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Status maps err to the metrics status: nil is success, a cancelled
// context is canceled, client-caused kinds are error, the rest failure.
func Status(err error) metrics.Status {
	switch {
	case err == nil:
		return metrics.StatusSuccess
	case !classified(err) && errors.Is(err, context.Canceled):
		return metrics.StatusCanceled
	}
	switch KindOf(err) {
	case KindInvalid, KindUnauthenticated, KindForbidden, KindNotFound, KindConflict:
		return metrics.StatusError
	default:
		return metrics.StatusFailure
	}
}

// StatusForHTTP maps an HTTP status code to the metrics status.
func StatusForHTTP(code int) metrics.Status {
	switch {
	case code == 499:
		return metrics.StatusCanceled
	case code >= 500:
		return metrics.StatusFailure
	case code >= 400:
		return metrics.StatusError
	default:
		return metrics.StatusSuccess
	}
}

// Body is the standard JSON error body every service returns.
type Body struct {
	Error BodyError `json:"error"`
}

// BodyError is Body's payload. Code is the Kind; CorrelationID lets a
// caller quote the request in a bug report.
type BodyError struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// WriteHTTP writes err as the standard error body. Internal errors get a
// generic message: details stay in the logs.
func WriteHTTP(w http.ResponseWriter, err error, correlationID string) {
	code := HTTPStatus(err)
	if code == 499 {
		// Nobody is listening; record the status without a body.
		w.WriteHeader(code)
		return
	}
	kind := KindOf(err)
	msg := "internal error"
	var e *Error
	if errors.As(err, &e) && kind != KindInternal {
		msg = e.Message
	}
	if code == http.StatusGatewayTimeout {
		kind, msg = "timeout", "request timed out"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(Body{Error: BodyError{Code: string(kind), Message: msg, CorrelationID: correlationID}})
}
