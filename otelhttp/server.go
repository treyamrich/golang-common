// Package otelhttp wraps the upstream
// go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp package with
// opinionated defaults: pre-configured exclusion of liveness/readiness and
// metrics scrape endpoints, a service-name option that flows into the
// upstream WithServerName setting, and emission of the standard
// api_counter / api_histogram metrics from the metrics package (so all
// services share one dashboard surface).
//
// Spans are intentionally NOT emitted in v0.x — we ship metrics only and
// will add a span hook in a later release without changing the public
// surface.
package otelhttp

import (
	"net/http"
	"strconv"
	"time"

	contribhttp "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/treyamrich/golang-common/log"
	"github.com/treyamrich/golang-common/metrics"
)

// CorrelationIDHeader is the canonical header used to propagate cxid
// across services. Server reads it (validating against log.IsValidCxid),
// generates one if absent/malformed, and echoes it back on the response.
// NewClient forwards it from context on outbound calls.
const CorrelationIDHeader = "X-Correlation-Id"

// DefaultExcludedPaths are stripped from instrumentation by default. These
// are the common-convention endpoints that would otherwise pollute the API
// metrics with high-cardinality, low-signal samples.
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
// as server.address on emitted upstream metrics (kept for trace context).
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

// WithUpstreamOptions appends raw upstream contribhttp.Options.
func WithUpstreamOptions(opts ...contribhttp.Option) ServerOption {
	return func(c *serverConfig) { c.upstreamOpts = append(c.upstreamOpts, opts...) }
}

// Server wraps an http.Handler with HTTP server instrumentation. For every
// non-excluded request it emits the standard tier metrics from the
// metrics package:
//
//	api_counter   labels: status, route, method, code + standard set
//	api_histogram labels: status, route, method, code + standard set (unit=seconds)
//
// Default excluded paths: /healthz, /readyz, /metrics.
func Server(h http.Handler, opts ...ServerOption) http.Handler {
	cfg := &serverConfig{
		excludedPaths: DefaultExcludedPaths,
		operationName: "http.server",
	}
	for _, o := range opts {
		o(cfg)
	}

	excluded := pathSet(cfg.excludedPaths)
	instrumented := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cxid extraction/injection runs on EVERY request — including
		// excluded paths — so a /healthz call that fans out to backend
		// services still carries a usable correlation id. The metric
		// emission below is what's path-gated.
		cxid := r.Header.Get(CorrelationIDHeader)
		if !log.IsValidCxid(cxid) {
			cxid = log.NewCxid()
		}
		ctx := log.WithCxid(r.Context(), cxid)
		r = r.WithContext(ctx)
		w.Header().Set(CorrelationIDHeader, cxid)

		if _, skip := excluded[r.URL.Path]; skip {
			h.ServeHTTP(w, r)
			return
		}
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		h.ServeHTTP(sw, r)
		dur := time.Since(start)
		status := classify(sw.status)
		opts := []metrics.APIOption{
			metrics.WithRoute(r.URL.Path),
			metrics.WithMethod(r.Method),
			metrics.WithCode(sw.status),
		}
		metrics.IncAPI(status, opts...)
		metrics.RecordAPILatency(dur, status, opts...)
	})

	upstream := []contribhttp.Option{
		contribhttp.WithFilter(excludeFilter(cfg.excludedPaths)),
	}
	if cfg.serviceName != "" {
		upstream = append(upstream, contribhttp.WithServerName(cfg.serviceName))
	}
	upstream = append(upstream, cfg.upstreamOpts...)
	return contribhttp.NewHandler(instrumented, cfg.operationName, upstream...)
}

func classify(code int) metrics.Status {
	switch {
	case code >= 500:
		return metrics.StatusFailure
	case code >= 400:
		return metrics.StatusError
	default:
		return metrics.StatusSuccess
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// statusString renders an HTTP code as its decimal string. Exposed for
// internal helpers (and to keep strconv usage in one place).
func statusString(code int) string { return strconv.Itoa(code) }

func pathSet(paths []string) map[string]struct{} {
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	return set
}

func excludeFilter(paths []string) contribhttp.Filter {
	if len(paths) == 0 {
		return func(*http.Request) bool { return true }
	}
	set := pathSet(paths)
	return func(r *http.Request) bool {
		_, excluded := set[r.URL.Path]
		return !excluded
	}
}
