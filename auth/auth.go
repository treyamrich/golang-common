// Package auth authenticates inbound requests and checks scopes.
//
// Every service gets the same behaviour: a bearer JWT from a trusted
// issuer (Authentik) with an accepted audience, verified against the
// issuer's JWKS. The verified caller is a Principal on the request
// context; handlers and core code read it with FromContext and never
// parse headers themselves.
//
// Which routes need auth, and which scopes, is declared in the service's
// OpenAPI spec (security: [{oidc: [tasks:write]}]). OpenAPI is the
// per-operation middleware oapi-codegen calls, and enforces exactly what
// the spec says. RequireScopes is the same check as a function, for the
// rare route that decides at runtime.
//
// Authorization stops at "may this caller perform this action on this
// type of thing" (a scope). Whether they may touch one particular object
// is the core's business, using Principal.Subject.
package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/treyamrich/golang-common/apperr"
	"github.com/treyamrich/golang-common/log"
)

// Principal is an authenticated caller.
type Principal struct {
	Subject  string
	Issuer   string
	Audience []string
	Email    string
	Name     string
	Username string
	Groups   []string
	Scopes   []string
	Expiry   time.Time
	// Claims holds every claim, for anything not lifted above.
	Claims map[string]any
}

// HasScopes reports whether p was granted every scope.
func (p Principal) HasScopes(scopes ...string) bool {
	for _, s := range scopes {
		if !slices.Contains(p.Scopes, s) {
			return false
		}
	}
	return true
}

// Verifier turns a raw bearer token into a Principal.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (Principal, error)
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p. Tests use it to fake a caller.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the authenticated caller, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// RequireScopes returns nil when ctx carries a principal holding every
// scope, apperr Unauthenticated when there is no principal, and apperr
// Forbidden naming the missing scopes otherwise.
func RequireScopes(ctx context.Context, scopes ...string) error {
	p, ok := FromContext(ctx)
	if !ok {
		return apperr.Unauthenticated("authentication required")
	}
	var missing []string
	for _, s := range scopes {
		if !slices.Contains(p.Scopes, s) {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return apperr.Forbidden("missing scope: %s", strings.Join(missing, " "))
	}
	return nil
}

// BearerToken extracts the token from an "Authorization: Bearer" header.
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(tok) == "" {
		return "", false
	}
	return strings.TrimSpace(tok), true
}

// Authenticate verifies r's bearer token and returns r's context with the
// principal added.
func Authenticate(r *http.Request, v Verifier) (context.Context, error) {
	tok, ok := BearerToken(r)
	if !ok {
		return nil, apperr.Unauthenticated("bearer token required")
	}
	p, err := v.Verify(r.Context(), tok)
	if err != nil {
		// The reason is for our logs only; the caller gets a plain 401.
		slog.InfoContext(r.Context(), "auth: token rejected", "reason", err.Error())
		if errors.Is(err, ErrIssuerUnavailable) {
			return nil, apperr.Wrap(err, apperr.KindUnavailable, "identity provider unavailable")
		}
		return nil, apperr.Wrap(err, apperr.KindUnauthenticated, "invalid token")
	}
	return WithPrincipal(r.Context(), p), nil
}

// Middleware requires a valid bearer token on every request (and the
// given scopes, if any). Use it to protect a whole handler; use OpenAPI
// for per-operation rules from the spec.
func Middleware(v Verifier, scopes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, err := Authenticate(r, v)
			if err == nil {
				err = RequireScopes(ctx, scopes...)
			}
			if err != nil {
				reject(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// OpenAPI enforces the security requirements oapi-codegen puts on each
// operation's context. Pass the generated scope keys (e.g.
// api.OidcScopes). An operation with a requirement gets authenticated and
// must hold its scopes. One declared without security (security: [] or
// no global default) is public, and is left alone.
//
// Wire it as the generated server's per-operation middleware:
//
//	api.HandlerWithOptions(strict, api.StdHTTPServerOptions{
//	    BaseRouter:  mux,
//	    Middlewares: []api.MiddlewareFunc{auth.OpenAPI(verifier, api.OidcScopes)},
//	})
func OpenAPI(v Verifier, scopeKeys ...any) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			required, secured := requiredScopes(r.Context(), scopeKeys)
			if !secured {
				next.ServeHTTP(w, r)
				return
			}
			ctx, err := Authenticate(r, v)
			if err == nil {
				err = RequireScopes(ctx, required...)
			}
			if err != nil {
				reject(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func requiredScopes(ctx context.Context, keys []any) ([]string, bool) {
	var out []string
	secured := false
	for _, k := range keys {
		if s, ok := ctx.Value(k).([]string); ok {
			secured = true
			out = append(out, s...)
		}
	}
	return out, secured
}

func reject(w http.ResponseWriter, r *http.Request, err error) {
	if apperr.Is(err, apperr.KindUnauthenticated) {
		w.Header().Set("WWW-Authenticate", `Bearer`)
	}
	apperr.WriteHTTP(w, err, log.FromContext(r.Context()))
}
