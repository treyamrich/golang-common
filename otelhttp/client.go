package otelhttp

import (
	"net/http"
	"time"

	contribhttp "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/treyamrich/golang-common/metrics"
)

// DefaultClientTimeout is applied to clients constructed by NewClient when
// WithTimeout is not provided. Outbound HTTP calls without a timeout can
// hang a process indefinitely — this default is intentionally conservative.
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
// upstream OTEL HTTP transport AND emits the standard tier metrics for
// every completed call:
//
//	api_counter   labels: status, route (URL path), method, code + standard set
//	api_histogram labels: status, route (URL path), method, code + standard set (seconds)
//
// IMPORTANT: spans are not emitted in v0.x — metrics only.
func NewClient(opts ...ClientOption) *http.Client {
	cfg := &clientConfig{timeout: DefaultClientTimeout}
	for _, o := range opts {
		o(cfg)
	}
	base := cfg.baseTransport
	if base == nil {
		base = http.DefaultTransport
	}
	wrapped := contribhttp.NewTransport(base, cfg.upstreamOpts...)
	return &http.Client{
		Timeout:   cfg.timeout,
		Transport: &metricsRoundTripper{next: wrapped},
	}
}

type metricsRoundTripper struct {
	next http.RoundTripper
}

func (m *metricsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := m.next.RoundTrip(req)
	dur := time.Since(start)

	route := ""
	if req.URL != nil {
		route = req.URL.Path
	}
	opts := []metrics.APIOption{
		metrics.WithRoute(route),
		metrics.WithMethod(req.Method),
	}
	var status metrics.Status
	if err != nil {
		status = metrics.StatusFailure
	} else {
		opts = append(opts, metrics.WithCode(resp.StatusCode))
		status = classify(resp.StatusCode)
	}
	metrics.IncAPI(status, opts...)
	metrics.RecordAPILatency(dur, status, opts...)
	return resp, err
}
