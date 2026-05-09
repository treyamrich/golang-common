# Changelog

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
