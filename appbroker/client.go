package appbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Defaults used by FromEnv.
const (
	// DefaultBaseURL is the broker's in-cluster Service address.
	DefaultBaseURL = "http://app-broker.app-broker.svc.cluster.local:8080"
	// DefaultTokenFile is where app-template's appBroker block mounts
	// the projected service-account token (audience "app-broker").
	DefaultTokenFile = "/var/run/secrets/app-broker/token"
)

// ClientOptions configures a Client. BaseURL and one credential (Token
// or TokenFile) are required.
type ClientOptions struct {
	// BaseURL is the broker root, e.g.
	// "http://app-broker.app-broker.svc.cluster.local:8080".
	BaseURL string

	// TokenFile is a projected service-account token (audience
	// app-broker) — how in-cluster services authenticate. The file is
	// re-read periodically because the kubelet rotates it. Takes
	// precedence over Token.
	TokenFile string

	// Token is a static consumer token, for callers outside the cluster
	// whose BrokerConsumer declares offClusterToken.
	Token string

	// HTTPClient is the underlying transport. If nil a new
	// *http.Client with Timeout=10s is created.
	HTTPClient *http.Client
}

// Client is the typed SDK over the broker's HTTP surface. Construct
// with NewClient or FromEnv. Safe for concurrent use as long as the
// underlying *http.Client is.
type Client struct {
	baseURL string
	token   string
	file    *tokenFile
	http    *http.Client
}

// NewClient constructs a Client. It does not perform any network I/O.
func NewClient(opts ClientOptions) *Client {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	c := &Client{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		token:   opts.Token,
		http:    hc,
	}
	if opts.TokenFile != "" {
		c.file = &tokenFile{path: opts.TokenFile}
	}
	return c
}

// FromEnv builds a Client from the environment app-template's appBroker
// block sets up:
//
//	APP_BROKER_URL         broker base URL (default DefaultBaseURL)
//	APP_BROKER_TOKEN_FILE  projected SA token (default DefaultTokenFile, if present)
//	APP_BROKER_TOKEN       static off-cluster token (used when no token file)
func FromEnv() (*Client, error) {
	base := os.Getenv("APP_BROKER_URL")
	if base == "" {
		base = DefaultBaseURL
	}
	opts := ClientOptions{BaseURL: base}
	if f := os.Getenv("APP_BROKER_TOKEN_FILE"); f != "" {
		opts.TokenFile = f
	} else if _, err := os.Stat(DefaultTokenFile); err == nil {
		opts.TokenFile = DefaultTokenFile
	} else {
		opts.Token = os.Getenv("APP_BROKER_TOKEN")
	}
	if opts.TokenFile == "" && opts.Token == "" {
		return nil, errors.New("app-broker: no credential: mount a projected token (APP_BROKER_TOKEN_FILE) or set APP_BROKER_TOKEN")
	}
	return NewClient(opts), nil
}

// tokenRefresh is how long a token read from TokenFile is reused before
// re-reading. The kubelet refreshes the file well before expiry (at 80%
// of its lifetime), so a minute of staleness is safe.
const tokenRefresh = time.Minute

type tokenFile struct {
	path string
	mu   sync.Mutex
	tok  string
	read time.Time
}

func (t *tokenFile) get() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tok != "" && time.Since(t.read) < tokenRefresh {
		return t.tok, nil
	}
	b, err := os.ReadFile(t.path)
	if err != nil {
		if t.tok != "" {
			return t.tok, nil // transient read failure: keep the last good token
		}
		return "", fmt.Errorf("app-broker: read token file: %w", err)
	}
	t.tok = strings.TrimSpace(string(b))
	t.read = time.Now()
	return t.tok, nil
}

func (c *Client) credential() (string, error) {
	if c.file != nil {
		return c.file.get()
	}
	return c.token, nil
}

// idTokenCtxKey is the context key for the per-request Authentik ID
// token. Unexported type prevents accidental collision with consumer
// code.
type idTokenCtxKey struct{}

// WithIDToken attaches a signed-in user's Authentik ID token to ctx.
// Requests made with ctx forward it as X-Homelab-Identity, so the broker
// acts for that user. Required for user actions (install, uninstall,
// listing the user's connections); the consumer must declare
// userSignIn.audiences.
func WithIDToken(ctx context.Context, idToken string) context.Context {
	return context.WithValue(ctx, idTokenCtxKey{}, idToken)
}

// idTokenFromContext returns the ID token attached by WithIDToken,
// or "" if none.
func idTokenFromContext(ctx context.Context) string {
	v, _ := ctx.Value(idTokenCtxKey{}).(string)
	return v
}

// ---------------------------------------------------------------------
// internal request plumbing
// ---------------------------------------------------------------------

// do builds a request, applies auth headers, sends it, and returns
// the *http.Response on 2xx. On non-2xx it parses the broker's
// {"error":..,"message":..} envelope and returns a typed *APIError.
//
// body may be nil (no body), or any value JSON-marshalable to write
// as the request body with Content-Type: application/json.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("app-broker: marshal body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, fmt.Errorf("app-broker: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	tok, err := c.credential()
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if idTok := idTokenFromContext(ctx); idTok != "" {
		req.Header.Set("X-Homelab-Identity", idTok)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}

	// Error path: drain body and build APIError. Always close.
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		sentinel:   classify(resp.StatusCode, ""),
	}
	if len(raw) > 0 {
		var env struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if jsonErr := json.Unmarshal(raw, &env); jsonErr == nil {
			apiErr.Code = env.Error
			apiErr.Message = env.Message
			apiErr.sentinel = classify(resp.StatusCode, env.Error)
		}
	}
	if apiErr.sentinel == nil {
		// Unknown / unmapped status — return APIError without a
		// sentinel so callers fall through to the default case.
		apiErr.sentinel = fmt.Errorf("app-broker: http %d", resp.StatusCode)
	}
	return nil, apiErr
}

// decodeJSON closes the response body and decodes it into out. Use
// only on successful (2xx) responses.
func decodeJSON(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("app-broker: decode response: %w", err)
	}
	return nil
}

// drainAndClose discards the body of resp and closes it. Used when
// we don't care about a 2xx response body.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
