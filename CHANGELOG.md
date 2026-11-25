# Changelog

## v0.5.0 — 2026-11-25

A service framework: every service gets the same bootstrap, HTTP chain,
auth, errors, metrics and logs. Reference service:
github.com/treyamrich/go-service-template.

### Added
- `app`: `Run` bootstrap with config, logging/metrics init, admin server
  (`/healthz`, `/readyz` from started dependencies, `/metrics`), ordered
  dependency start/close, runners, `Every`/`Once`/`RunJob` job loops
  (cxid per run, `job_histogram`, panic recovery) and graceful shutdown.
- `config`: `Load[T]` from env vars (caarlos0/env).
- `api`: HTTP server and standard chain: correlation ID, `api_histogram`
  + access log with route-pattern labels, client disconnect recorded as
  499/canceled, panic recovery, request timeout, body limit; strict-server
  error handlers.
- `auth`: OIDC JWT verification (moved and generalised from app-broker),
  `Principal`, `OpenAPI` per-operation middleware enforcing the spec's
  security/scopes, `Middleware`, `RequireScopes`; `auth/authtest` fake
  issuer.
- `apperr`: error kinds → HTTP status, metrics status, standard body.
- `observe`: `Call`/`Do` dependency wrapper → `dependency_histogram`.
- `postgres`: pgx pool as an `app` dependency.
- `metrics`: `dependency_histogram`, `job_histogram`, `StatusCanceled`.
- `otelinit`: Prometheus exporter (default) and `Handler()`.
- `log`: `ContextHandler`, so `slog.*Context` lines carry the cxid;
  `CorrelationIDHeader`.

### Changed (breaking)
- `api_histogram` labels are `api`, `http_status`, `status`; `api_counter`
  removed (use `_count`).
- `otelhttp.NewClient` records `dependency_histogram`
  (`WithDependency`), not `api_*`.
- Removed `otelhttp.Server`, `metrics.IncAPI`, `RecordAPILatency`,
  `WithRoute`/`WithMethod`/`WithCode`.

## v0.4.0 — 2026-11-24

### Added
- `appbroker/` package: the app-broker client SDK, moved here from
  app-broker's `pkg/client` so any service can import it (app-broker's
  repo is private). Typed methods for every broker endpoint, `FromEnv`
  (projected service-account token or static token), `ForEachInstall`,
  `TokenCache`, `WithIDToken`, and sentinel errors.

### Changed
- (None for existing packages.) Compared to app-broker's `pkg/client`,
  the deprecated `ClientOptions.Mode` / `ModeA` / `ModeB` are gone: a
  sign-in is forwarded whenever the context carries one.

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
