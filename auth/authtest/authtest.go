// Package authtest is a fake OIDC issuer for tests: it serves discovery
// and a JWKS, and mints RS256 tokens the auth package will accept.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Issuer is a running fake issuer.
type Issuer struct {
	URL      string // issuer URL, with Authentik's trailing slash
	Audience string // default aud for minted tokens
	key      *rsa.PrivateKey
	kid      string
	srv      *httptest.Server
}

// New starts an issuer that is stopped when the test ends.
func New(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &Issuer{key: key, kid: "test-key", Audience: "test-client"}
	mux := http.NewServeMux()
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	is.URL = is.srv.URL + "/application/o/test/"
	base := strings.TrimRight(is.URL, "/")
	mux.HandleFunc("/application/o/test/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is.URL, "jwks_uri": base + "/jwks/"})
	})
	mux.HandleFunc("/application/o/test/jwks/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &is.key.PublicKey, KeyID: is.kid, Algorithm: "RS256", Use: "sig"},
		}})
	})
	return is
}

// Claims for Token. Zero fields get defaults: Subject "user-1",
// Audience Issuer.Audience, expiry in 5 minutes.
type Claims struct {
	Subject  string
	Audience string
	Scopes   []string
	Email    string
	Groups   []string
	Expiry   time.Time
	Extra    map[string]any
}

// Token mints a signed token.
func (is *Issuer) Token(t testing.TB, c Claims) string {
	t.Helper()
	if c.Subject == "" {
		c.Subject = "user-1"
	}
	if c.Audience == "" {
		c.Audience = is.Audience
	}
	if c.Expiry.IsZero() {
		c.Expiry = time.Now().Add(5 * time.Minute)
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: is.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", is.kid))
	if err != nil {
		t.Fatal(err)
	}
	priv := map[string]any{"scope": strings.Join(c.Scopes, " ")}
	if c.Email != "" {
		priv["email"] = c.Email
	}
	if c.Groups != nil {
		priv["groups"] = c.Groups
	}
	for k, v := range c.Extra {
		priv[k] = v
	}
	tok, err := jwt.Signed(sig).Claims(jwt.Claims{
		Issuer:   is.URL,
		Subject:  c.Subject,
		Audience: jwt.Audience{c.Audience},
		Expiry:   jwt.NewNumericDate(c.Expiry),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}).Claims(priv).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
