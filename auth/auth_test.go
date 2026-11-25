package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/treyamrich/golang-common/apperr"
	"github.com/treyamrich/golang-common/auth"
	"github.com/treyamrich/golang-common/auth/authtest"
)

func newVerifier(t *testing.T, is *authtest.Issuer) *auth.OIDC {
	t.Helper()
	v, err := auth.NewOIDC(auth.OIDCConfig{Issuers: []string{is.URL}, Audiences: []string{is.Audience}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerify(t *testing.T) {
	is := authtest.New(t)
	other := authtest.New(t)
	v := newVerifier(t, is)
	ctx := context.Background()

	p, err := v.Verify(ctx, is.Token(t, authtest.Claims{Scopes: []string{"tasks:read", "openid"}, Email: "a@b"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "user-1" || p.Email != "a@b" || !p.HasScopes("tasks:read") || p.HasScopes("tasks:write") {
		t.Fatalf("principal %+v", p)
	}

	cases := map[string]struct {
		tok  string
		want error
	}{
		"garbage":       {"not-a-jwt", auth.ErrInvalidToken},
		"wrong aud":     {is.Token(t, authtest.Claims{Audience: "someone-else"}), auth.ErrAudMismatch},
		"expired":       {is.Token(t, authtest.Claims{Expiry: time.Now().Add(-time.Hour)}), auth.ErrExpiredToken},
		"untrusted iss": {other.Token(t, authtest.Claims{}), auth.ErrIssuerMismatch},
	}
	for name, c := range cases {
		if _, err := v.Verify(ctx, c.tok); !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v want %v", name, err, c.want)
		}
	}
}

func TestVerifyIssuerDownIsUnavailable(t *testing.T) {
	is := authtest.New(t)
	v, _ := auth.NewOIDC(auth.OIDCConfig{Issuers: []string{is.URL}, Audiences: []string{is.Audience},
		HTTPClient: &http.Client{Transport: failingRT{}}})
	if _, err := v.Verify(context.Background(), is.Token(t, authtest.Claims{})); !errors.Is(err, auth.ErrIssuerUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

type failingRT struct{}

func (failingRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New("down") }

func TestNewOIDCRequiresIssuerAndAudience(t *testing.T) {
	if _, err := auth.NewOIDC(auth.OIDCConfig{Audiences: []string{"x"}}); err == nil {
		t.Error("no issuer accepted")
	}
	if _, err := auth.NewOIDC(auth.OIDCConfig{Issuers: []string{"https://x"}}); err == nil {
		t.Error("no audience accepted")
	}
}

func TestRequireScopes(t *testing.T) {
	if err := auth.RequireScopes(context.Background(), "a"); !apperr.Is(err, apperr.KindUnauthenticated) {
		t.Errorf("no principal: %v", err)
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Scopes: []string{"a", "b"}})
	if err := auth.RequireScopes(ctx, "a", "b"); err != nil {
		t.Errorf("has scopes: %v", err)
	}
	if err := auth.RequireScopes(ctx, "a", "c"); !apperr.Is(err, apperr.KindForbidden) {
		t.Errorf("missing scope: %v", err)
	}
}

const scopesKey = "oidc.Scopes" // what oapi-codegen generates

func TestOpenAPIMiddleware(t *testing.T) {
	is := authtest.New(t)
	mw := auth.OpenAPI(newVerifier(t, is), scopesKey)
	var gotSub string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.FromContext(r.Context())
		gotSub = p.Subject
	}))
	call := func(required []string, token string) int {
		req := httptest.NewRequest("GET", "/", nil)
		if required != nil {
			req = req.WithContext(context.WithValue(req.Context(), scopesKey, required)) //nolint:staticcheck // mirrors oapi-codegen's string key
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	read := is.Token(t, authtest.Claims{Scopes: []string{"tasks:read"}})

	if c := call(nil, ""); c != 200 {
		t.Errorf("public route: %d", c)
	}
	if c := call([]string{"tasks:read"}, ""); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c := call([]string{"tasks:read"}, "junk"); c != 401 {
		t.Errorf("bad token: %d", c)
	}
	if c := call([]string{"tasks:write"}, read); c != 403 {
		t.Errorf("missing scope: %d", c)
	}
	if c := call([]string{}, read); c != 200 || gotSub != "user-1" {
		t.Errorf("auth-only route: %d sub=%q", c, gotSub)
	}
	if c := call([]string{"tasks:read"}, read); c != 200 {
		t.Errorf("scoped route: %d", c)
	}
}

func TestMiddleware(t *testing.T) {
	is := authtest.New(t)
	h := auth.Middleware(newVerifier(t, is), "admin")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+is.Token(t, authtest.Claims{Scopes: []string{"admin"}}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("code=%d", rec.Code)
	}
}
