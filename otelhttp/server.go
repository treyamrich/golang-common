// Package otelhttp wraps the upstream
// go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp package with
// homelab-opinionated defaults: pre-configured exclusion of liveness/readiness
// and metrics scrape endpoints, and a service-name option that flows into
// the upstream WithServerName setting.
//
// Spans are intentionally NOT emitted in v0.1.0 — we ship metrics only and
// will add a span hook later without changing the public surface. Metric
// instrumentation comes directly from the upstream package, so we do NOT
// reinvent instruments.
package otelhttp

import (
	"net/http"

	contribhttp "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// DefaultExcludedPaths are stripped from instrumentation by default. These
// are the homelab convention endpoints that would otherwise pollute the
// http.server.* histogram with high-cardinality, low-signal samples.
var DefaultExcludedPaths = []string{"/healthz", "/readyz", "/metrics"}

type serverConfig struct {
	serviceName   string
	excludedPaths []string
	operationName string
	upstreamOpts  []contribhttp.Option
}

// ServerOption configures Server.
type ServerOption func(*serverConfig)

// WithServiceName sets the upstream WithServerName attribute, which appears
// as server.address on emitted metrics.
func WithServiceName(name string) ServerOption {
	return func(c *serverConfig) { c.serviceName = name }
}

// WithExcludedPaths overrides DefaultExcludedPaths. Pass an empty slice
// (not nil) to instrument every request including health probes.
func WithExcludedPaths(paths ...string) ServerOption {
	return func(c *serverConfig) { c.excludedPaths = paths }
}

// WithOperationName overrides the operation name passed to the upstream
// handler. Default: "http.server".
func WithOperationName(op string) ServerOption {
	return func(c *serverConfig) { c.operationName = op }
}

// WithUpstreamOptions appends raw upstream contribhttp.Options. Use as an
// escape hatch when you need an upstream feature we haven't surfaced.
func WithUpstreamOptions(opts ...contribhttp.Option) ServerOption {
	return func(c *serverConfig) { c.upstreamOpts = append(c.upstreamOpts, opts...) }
}

// Server wraps an http.Handler with OTEL HTTP server instrumentation per
// the OTEL semantic conventions. Emitted metrics:
//
//	http.server.request.duration  (histogram, seconds)
//	http.server.active_requests   (UpDownCounter)
//
// Labels follow conventions: http.request.method, http.response.status_code,
// http.route, server.address.
//
// Default excluded paths: /healthz, /readyz, /metrics. Override via
// WithExcludedPaths.
func Server(h http.Handler, opts ...ServerOption) http.Handler {
	cfg := &serverConfig{
		excludedPaths: DefaultExcludedPaths,
		operationName: "http.server",
	}
	for _, o := range opts {
		o(cfg)
	}

	upstream := []contribhttp.Option{
		contribhttp.WithFilter(excludeFilter(cfg.excludedPaths)),
	}
	if cfg.serviceName != "" {
		upstream = append(upstream, contribhttp.WithServerName(cfg.serviceName))
	}
	upstream = append(upstream, cfg.upstreamOpts...)

	return contribhttp.NewHandler(h, cfg.operationName, upstream...)
}

func excludeFilter(paths []string) contribhttp.Filter {
	if len(paths) == 0 {
		return func(*http.Request) bool { return true }
	}
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	return func(r *http.Request) bool {
		_, excluded := set[r.URL.Path]
		return !excluded
	}
}
