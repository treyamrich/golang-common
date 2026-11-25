package otelhttp

import (
	"net/http"
	"time"

	contribhttp "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/treyamrich/golang-common/log"
	"github.com/treyamrich/golang-common/metrics"
	"github.com/treyamrich/golang-common/observe"
)

// DefaultClientTimeout is applied to clients constructed by NewClient when
// WithTimeout is not provided. Outbound HTTP calls without a timeout can
// hang a process indefinitely — this default is intentionally conservative.
const DefaultClientTimeout = 10 * time.Second

// CorrelationIDHeader carries the cxid between services.
const CorrelationIDHeader = log.CorrelationIDHeader

type clientConfig struct {
	dependency    string
	timeout       time.Duration
	baseTransport http.RoundTripper
	upstreamOpts  []contribhttp.Option
}

// ClientOption configures NewClient.
type ClientOption func(*clientConfig)

// WithDependency names the dependency for the dependency_histogram
// label ("spotify", "app-broker"). Default: the request URL's host.
func WithDependency(name string) ClientOption {
	return func(c *clientConfig) { c.dependency = name }
}

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

// NewClient returns an *http.Client for calling a dependency. Every call
// forwards the context's cxid as X-Correlation-Id and records one
// dependency_histogram sample{dependency, operation=<HTTP method>,
// status}. A call cancelled by our own context is "canceled", not a
// failure.
//
// Spans are not emitted in v0.x — metrics only.
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
		Transport: &metricsRoundTripper{next: wrapped, dependency: cfg.dependency},
	}
}

type metricsRoundTripper struct {
	next       http.RoundTripper
	dependency string
}

func (m *metricsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Forward cxid from context onto the outbound request — but never
	// override a header the caller already set, in case it's intentional
	// (e.g., replaying or stitching trace).
	if req.Header.Get(CorrelationIDHeader) == "" {
		if cxid := log.FromContext(req.Context()); cxid != "" {
			// Clone before mutating to avoid surprising the caller.
			req = req.Clone(req.Context())
			req.Header.Set(CorrelationIDHeader, cxid)
		}
	}

	start := time.Now()
	resp, err := m.next.RoundTrip(req)
	dep := m.dependency
	if dep == "" && req.URL != nil {
		dep = req.URL.Hostname()
	}
	var status metrics.Status
	if err != nil {
		status = observe.DefaultStatus(req.Context(), err)
	} else {
		status = classify(resp.StatusCode)
	}
	metrics.RecordDependency(dep, req.Method, status, time.Since(start))
	return resp, err
}

// classify maps a dependency's response code to our view of the call:
// its 5xx and 429 (it couldn't serve us) are failures, other 4xx are
// errors (usually our request was wrong), the rest success.
func classify(code int) metrics.Status {
	switch {
	case code >= 500 || code == http.StatusTooManyRequests:
		return metrics.StatusFailure
	case code >= 400:
		return metrics.StatusError
	default:
		return metrics.StatusSuccess
	}
}
