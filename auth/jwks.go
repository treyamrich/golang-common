package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Verification failures. Callers answer every one of them with a plain
// 401 and log the specific one; never tell a client which check failed.
var (
	ErrInvalidToken      = errors.New("auth: invalid signature or format")
	ErrExpiredToken      = errors.New("auth: token expired")
	ErrIssuerMismatch    = errors.New("auth: untrusted issuer")
	ErrAudMismatch       = errors.New("auth: aud not accepted")
	ErrIssuerUnavailable = errors.New("auth: issuer discovery unavailable")
)

const (
	defaultRefreshInterval = time.Hour
	defaultHTTPTimeout     = 10 * time.Second
	clockSkew              = 5 * time.Minute
)

// jwksVerifier checks tokens from one issuer against its JWKS. Keys are
// fetched lazily, refreshed every refreshInterval, and refetched once
// when a token names an unknown kid (Authentik rotates keys without
// notice). Safe for concurrent use.
type jwksVerifier struct {
	issuer          string // trailing slash trimmed
	jwksURL         string
	httpClient      *http.Client
	refreshInterval time.Duration
	now             func() time.Time

	keys          atomic.Pointer[jose.JSONWebKeySet]
	fetchMu       sync.Mutex
	lastFetchedAt time.Time
}

func discoverJWKS(ctx context.Context, c *http.Client, issuer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery: status %d", resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("discovery: %w", err)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("discovery: jwks_uri missing")
	}
	return doc.JWKSURI, nil
}

func (v *jwksVerifier) fetch(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	var ks jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ks); err != nil {
		return fmt.Errorf("jwks: %w", err)
	}
	v.keys.Store(&ks)
	v.lastFetchedAt = v.now()
	return nil
}

func (v *jwksVerifier) keySet(ctx context.Context) (*jose.JSONWebKeySet, error) {
	if ks := v.keys.Load(); ks != nil && v.now().Sub(v.lastFetchedAt) < v.refreshInterval {
		return ks, nil
	}
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	if ks := v.keys.Load(); ks != nil && v.now().Sub(v.lastFetchedAt) < v.refreshInterval {
		return ks, nil
	}
	if err := v.fetch(ctx); err != nil {
		if ks := v.keys.Load(); ks != nil {
			return ks, nil // stale beats refusing everyone during a JWKS blip
		}
		return nil, err
	}
	return v.keys.Load(), nil
}

func (v *jwksVerifier) refetchFor(ctx context.Context, kid string) (*jose.JSONWebKeySet, error) {
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	if ks := v.keys.Load(); ks != nil && len(ks.Key(kid)) > 0 {
		return ks, nil
	}
	if err := v.fetch(ctx); err != nil {
		return nil, err
	}
	return v.keys.Load(), nil
}

// claims is what a token carries. Authentik puts the granted scopes in
// "scope" (space-separated); other issuers use "scp".
type claims struct {
	jwt.Claims
	Email             string          `json:"email"`
	Name              string          `json:"name"`
	PreferredUsername string          `json:"preferred_username"`
	Groups            []string        `json:"groups"`
	Scope             string          `json:"scope"`
	Scp               json.RawMessage `json:"scp"`
}

func (c claims) scopes() []string {
	out := strings.Fields(c.Scope)
	if len(c.Scp) > 0 {
		var list []string
		var one string
		if json.Unmarshal(c.Scp, &list) == nil {
			out = append(out, list...)
		} else if json.Unmarshal(c.Scp, &one) == nil {
			out = append(out, strings.Fields(one)...)
		}
	}
	return out
}

func (v *jwksVerifier) verify(ctx context.Context, parsed *jwt.JSONWebToken, audiences []string) (Principal, error) {
	if len(parsed.Headers) != 1 || parsed.Headers[0].KeyID == "" {
		return Principal{}, ErrInvalidToken
	}
	kid := parsed.Headers[0].KeyID
	ks, err := v.keySet(ctx)
	if err != nil {
		return Principal{}, ErrIssuerUnavailable
	}
	keys := ks.Key(kid)
	if len(keys) == 0 {
		if ks, err = v.refetchFor(ctx, kid); err != nil {
			return Principal{}, ErrInvalidToken
		}
		if keys = ks.Key(kid); len(keys) == 0 {
			return Principal{}, ErrInvalidToken
		}
	}
	var c claims
	var raw map[string]any
	verified := false
	for _, k := range keys {
		if parsed.Claims(k.Key, &c, &raw) == nil {
			verified = true
			break
		}
	}
	if !verified {
		return Principal{}, ErrInvalidToken
	}

	now := v.now()
	if c.Expiry == nil {
		return Principal{}, ErrInvalidToken
	}
	if !now.Before(c.Expiry.Time().Add(clockSkew)) {
		return Principal{}, ErrExpiredToken
	}
	if c.IssuedAt != nil && c.IssuedAt.Time().After(now.Add(clockSkew)) {
		return Principal{}, ErrInvalidToken
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimRight(c.Issuer, "/")), []byte(v.issuer)) != 1 {
		return Principal{}, ErrIssuerMismatch
	}
	if !audienceOK(c.Audience, audiences) {
		return Principal{}, ErrAudMismatch
	}
	return Principal{
		Subject:  c.Subject,
		Issuer:   c.Issuer,
		Audience: []string(c.Audience),
		Email:    c.Email,
		Name:     c.Name,
		Username: c.PreferredUsername,
		Groups:   c.Groups,
		Scopes:   c.scopes(),
		Expiry:   c.Expiry.Time(),
		Claims:   raw,
	}, nil
}

func audienceOK(got jwt.Audience, accepted []string) bool {
	for _, want := range accepted {
		for _, g := range got {
			if subtle.ConstantTimeCompare([]byte(g), []byte(want)) == 1 {
				return true
			}
		}
	}
	return false
}
