package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// OIDCConfig configures an OIDC verifier. The env tags let a service
// embed it in its config struct.
type OIDCConfig struct {
	// Issuers are the trusted issuer URLs, e.g.
	// https://auth.ts.lan.local/application/o/<app-slug>/ (Authentik
	// gives every application its own issuer).
	Issuers []string `env:"AUTH_ISSUERS" envSeparator:","`
	// Audiences are the accepted "aud" values: the OAuth client IDs whose
	// tokens this service accepts. Required: a token minted for some
	// other client must not work here.
	Audiences []string `env:"AUTH_AUDIENCES" envSeparator:","`
	// HTTPClient is used for discovery and JWKS. Default: 10s timeout,
	// system roots (mount the homelab CA and set SSL_CERT_DIR).
	HTTPClient *http.Client `env:"-"`
	// RefreshInterval bounds how stale cached keys may get. Default 1h.
	RefreshInterval time.Duration `env:"AUTH_JWKS_REFRESH" envDefault:"1h"`
	// DiscoveryRetry is the minimum gap between discovery attempts for
	// an issuer that failed. Default 30s.
	DiscoveryRetry time.Duration `env:"AUTH_DISCOVERY_RETRY" envDefault:"30s"`
}

// OIDC verifies RS256 JWTs (Authentik access or ID tokens) from a set of
// issuers. Discovery is lazy: an IdP outage at startup only fails
// authentication for that issuer until it recovers, it doesn't keep the
// service down. Safe for concurrent use.
type OIDC struct {
	cfg     OIDCConfig
	now     func() time.Time
	issuers map[string]bool

	mu        sync.Mutex
	verifiers map[string]*jwksVerifier
	lastTry   map[string]time.Time
}

// NewOIDC returns a verifier for cfg. It does no I/O; call Warm to try
// discovery up front.
func NewOIDC(cfg OIDCConfig) (*OIDC, error) {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = defaultRefreshInterval
	}
	if cfg.DiscoveryRetry <= 0 {
		cfg.DiscoveryRetry = 30 * time.Second
	}
	o := &OIDC{cfg: cfg, now: time.Now, issuers: map[string]bool{},
		verifiers: map[string]*jwksVerifier{}, lastTry: map[string]time.Time{}}
	for _, iss := range cfg.Issuers {
		if iss = strings.TrimRight(strings.TrimSpace(iss), "/"); iss != "" {
			o.issuers[iss] = true
		}
	}
	if len(o.issuers) == 0 {
		return nil, errors.New("auth: at least one issuer is required")
	}
	if len(cfg.Audiences) == 0 {
		return nil, errors.New("auth: at least one audience is required")
	}
	return o, nil
}

// Warm attempts discovery for every issuer and returns the failures for
// logging. Failed issuers are retried on use.
func (o *OIDC) Warm(ctx context.Context) []error {
	var errs []error
	for iss := range o.issuers {
		if _, err := o.verifier(ctx, iss, true); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// Verify implements Verifier.
func (o *OIDC) Verify(ctx context.Context, rawToken string) (Principal, error) {
	parsed, err := jwt.ParseSigned(rawToken, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return Principal{}, ErrInvalidToken
	}
	// The unverified issuer only picks which trusted key set verifies
	// the token; a forged iss lands on no verifier or fails signature.
	var c jwt.Claims
	if err := parsed.UnsafeClaimsWithoutVerification(&c); err != nil {
		return Principal{}, ErrInvalidToken
	}
	iss := strings.TrimRight(c.Issuer, "/")
	if !o.issuers[iss] {
		return Principal{}, ErrIssuerMismatch
	}
	v, err := o.verifier(ctx, iss, false)
	if err != nil {
		return Principal{}, ErrIssuerUnavailable
	}
	return v.verify(ctx, parsed, o.cfg.Audiences)
}

func (o *OIDC) verifier(ctx context.Context, iss string, force bool) (*jwksVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if v, ok := o.verifiers[iss]; ok {
		return v, nil
	}
	if last, tried := o.lastTry[iss]; tried && !force && o.now().Sub(last) < o.cfg.DiscoveryRetry {
		return nil, fmt.Errorf("auth: issuer %s: discovery failed recently", iss)
	}
	o.lastTry[iss] = o.now()
	jwksURL, err := discoverJWKS(ctx, o.cfg.HTTPClient, iss)
	if err != nil {
		return nil, fmt.Errorf("auth: issuer %s: %w", iss, err)
	}
	v := &jwksVerifier{issuer: iss, jwksURL: jwksURL, httpClient: o.cfg.HTTPClient,
		refreshInterval: o.cfg.RefreshInterval, now: o.now}
	o.verifiers[iss] = v
	return v, nil
}
