package otelhttp

import (
	"net/http"
	"time"

	contribhttp "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// DefaultClientTimeout is applied to clients constructed by NewClient when
// WithTimeout is not provided. Homelab services should never issue an HTTP
// call without a timeout — this default is intentionally conservative.
const DefaultClientTimeout = 10 * time.Second

type clientConfig struct {
	timeout       time.Duration
	baseTransport http.RoundTripper
	upstreamOpts  []contribhttp.Option
}

// ClientOption configures NewClient.
type ClientOption func(*clientConfig)

// WithTimeout overrides DefaultClientTimeout.
func WithTimeout(d time.Duration) ClientOption {
	return func(c *clientConfig) { c.timeout = d }
}

// WithBaseTransport sets the http.RoundTripper that the OTEL transport wraps.
// Use this for chained middleware (auth, retry, etc).
func WithBaseTransport(rt http.RoundTripper) ClientOption {
	return func(c *clientConfig) { c.baseTransport = rt }
}

// WithClientUpstreamOptions appends raw upstream contribhttp.Options.
func WithClientUpstreamOptions(opts ...contribhttp.Option) ClientOption {
	return func(c *clientConfig) { c.upstreamOpts = append(c.upstreamOpts, opts...) }
}

// NewClient returns an *http.Client whose Transport is wrapped with the
// upstream OTEL HTTP transport. Emitted metrics:
//
//	http.client.request.duration  (histogram, seconds)
//	http.client.request.body.size (counter)
//
// Labels follow conventions: http.request.method, http.response.status_code,
// server.address.
//
// IMPORTANT: spans are not emitted in v0.1.0 — metrics only.
func NewClient(opts ...ClientOption) *http.Client {
	cfg := &clientConfig{timeout: DefaultClientTimeout}
	for _, o := range opts {
		o(cfg)
	}
	base := cfg.baseTransport
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{
		Timeout:   cfg.timeout,
		Transport: contribhttp.NewTransport(base, cfg.upstreamOpts...),
	}
}
