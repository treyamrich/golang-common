# Changelog

## v0.2.0 — 2026-05-09

### Added
- `metrics/` package: `IncAPI`, `RecordAPILatency`, `IncCounter`, `RecordHistogram`,
  `Status` enum, `WithRoute/WithMethod/WithCode` options.
- `health/` package: `Handler` and `ReadinessHandler` for /healthz and /readyz.
- Auto-detected standard labels: pod, namespace, host, ip, cluster, region
  (read from k8s downward API + cloud env vars at init).

### Changed
- `otelhttp.Server` and `otelhttp.NewClient` now emit `api_counter` +
  `api_histogram` instead of OTEL semconv HTTP metrics. The public
  middleware/client API is unchanged; metric names in your dashboards
  will move.
- `otelinit.Config` gains `Environment` (now required), `Cluster`,
  `Region`, `StaticLabels`.

### Removed
- All "homelab"-specific language in docs, package comments, README.
  Library is now deployment-context-agnostic.

### Decisions documented in README
- `version` is intentionally NOT a standard metric label.

## v0.1.0 — 2026-05-08

Initial release.

- `otelinit.Init`: OTLP gRPC metric exporter setup (5s flush timeout), W3C
  tracecontext + baggage propagators, resource attributes (`service.name`,
  `service.version`, `service.instance.id`, `host.name`).
- `otelhttp.Server`: HTTP server middleware (request duration histogram +
  active requests up-down counter), default path excludes for `/healthz`,
  `/readyz`, `/metrics`.
- `otelhttp.NewClient`: HTTP client with instrumented transport (request
  duration histogram + body size counter), enforced default 10s timeout.
- Metrics-only (no spans). Span hooks added in a later minor without
  changing the public surface.
