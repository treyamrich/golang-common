package appbroker

import (
	"errors"
	"fmt"
)

// Sentinel errors for the meaningful broker outcomes. Use errors.Is
// to branch — every typed error returned by Client methods wraps
// (via APIError.Unwrap) one of these.
//
// The codes mirror the broker's JSON error envelope (see
// internal/http/errors.go: {"error":"<code>","message":"..."}). A
// generic ErrUnauthorized covers any 401 that is NOT consent_revoked.
var (
	// ErrUnauthorized is any 401 that is NOT consent_revoked
	// (missing/invalid bearer, invalid_id_token, etc.).
	ErrUnauthorized = errors.New("app-broker: unauthorized")

	// ErrForbidden is any 403 (mode_not_allowed, not_allowed,
	// not_admin).
	ErrForbidden = errors.New("app-broker: forbidden")

	// ErrNotFound is 404 — typically not_consented from /v1/token
	// when no connection exists yet.
	ErrNotFound = errors.New("app-broker: not found")

	// ErrConflict is 409. The current broker does not return 409
	// from any §6 endpoint, but the sentinel is reserved per spec
	// for forward compatibility.
	ErrConflict = errors.New("app-broker: conflict")

	// ErrConsentRevoked is the §6.10 specific 401 "consent_revoked"
	// case. The broker has wiped the Vault refresh record; the
	// consumer should call AuthStart to re-consent.
	ErrConsentRevoked = errors.New("app-broker: consent revoked")

	// ErrUpstreamUnavailable is 502 provider_unavailable / 503 from
	// the broker indicating the upstream provider (Spotify, etc.)
	// is down. Retry with backoff.
	ErrUpstreamUnavailable = errors.New("app-broker: upstream unavailable")
)

// APIError is the typed error returned by every Client method when
// the broker returns a non-2xx response. It carries the HTTP status,
// the broker's error code+message, and wraps a sentinel for
// errors.Is branching.
type APIError struct {
	StatusCode int    // HTTP status code
	Code       string // value of the "error" field in the JSON body
	Message    string // value of the "message" field, if any
	sentinel   error  // wrapped sentinel for errors.Is
}

// Error implements error. Format: "app-broker: <code> (<status>): <message>".
func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("app-broker: %s (%d): %s", e.Code, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("app-broker: %s (%d)", e.Code, e.StatusCode)
}

// Unwrap exposes the wrapped sentinel for errors.Is.
func (e *APIError) Unwrap() error { return e.sentinel }

// classify maps (status, code) → sentinel. Pure; no side effects.
func classify(status int, code string) error {
	switch status {
	case 401:
		if code == "consent_revoked" {
			return ErrConsentRevoked
		}
		return ErrUnauthorized
	case 403:
		return ErrForbidden
	case 404:
		return ErrNotFound
	case 409:
		return ErrConflict
	case 502, 503:
		return ErrUpstreamUnavailable
	}
	return nil
}
