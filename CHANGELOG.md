# Changelog

## v0.3.0 — 2026-05-09

### Added
- `log/` package: structured slog setup (`Init` writes JSON+RFC3339Nano to
  stderr, idempotent, env `LOG_LEVEL` override), cxid context helpers
  (`WithCxid`, `FromContext`, `NewCxid`, `IsValidCxid`), and a
  request-scoped logger (`New(ctx)`) that auto-attaches the cxid.
- `otelhttp.Server`: extracts inbound `X-Correlation-Id` (validated
  against `^[a-f0-9]{32}$`), generates a fresh cxid if absent or
  malformed, injects it into the request context via `log.WithCxid`,
  and echoes it on the response. Cxid runs on every request including
  excluded paths so health checks still carry one.
- `otelhttp.NewClient`: forwards cxid from context as `X-Correlation-Id`
  on outbound calls, only when the caller hasn't already set the header.
- `otelhttp.CorrelationIDHeader` constant (`"X-Correlation-Id"`).

### Changed
- (None — backward-compatible with v0.2.0 callers; cxid features are
  opt-in by importing `log/`. The only on-the-wire change is that
  `otelhttp.Server` now adds an `X-Correlation-Id` response header.)

### Decisions documented in README
- `cxid` is intentionally NOT a metric label (would explode cardinality).
  A regression test asserts neither `api_counter` nor `api_histogram`
  carries a `cxid` attribute.
- Bad `LOG_LEVEL` is coerced to `info` with a one-time warning rather
  than panicking — boot ergonomics matter more than strictness.
- Trace-id-on-logs is deferred to whichever release adds span emission,
  consistent with the v0.2.0 metrics-only stance.

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
