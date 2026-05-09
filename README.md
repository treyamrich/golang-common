# golang-common

Shared Go libraries for services consuming this module. Ships an
OpenTelemetry metrics baseline (HTTP server middleware, HTTP client
wrapper), an OTEL init helper, a small standard-and-adhoc metrics API,
and uniform health endpoints. Tracing is intentionally deferred to a
later release.

## Install

```sh
go get github.com/treyamrich/golang-common/otelinit@v0.2.0
go get github.com/treyamrich/golang-common/otelhttp@v0.2.0
go get github.com/treyamrich/golang-common/metrics@v0.2.0
go get github.com/treyamrich/golang-common/health@v0.2.0
```

## Packages

### `otelinit` — one-shot OTEL SDK setup

```go
shutdown, err := otelinit.Init(ctx, otelinit.Config{
    ServiceName:    "app-broker",
    ServiceVersion: version,
    Environment:    "prod",          // required
    Cluster:        "prod-us-east",  // optional
    Region:         "us-east-1",     // optional
    OTLPEndpoint:   os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
    Insecure:       true,
})
if err != nil {
    log.Fatal(err)
}
defer shutdown(context.Background())
```

`Init` wires the global `MeterProvider` with an OTLP gRPC exporter, the
W3C `tracecontext` + `baggage` propagators, and configures the standard
metric label set (see below). Returned `shutdown` flushes pending metrics
with a 5s timeout — defer it in `main`.

### `otelhttp` — HTTP server middleware + client wrapper

```go
mux := http.NewServeMux()
mux.HandleFunc("/v1/credential/oauth-user", handleOAuthUser)

server := &http.Server{
    Addr:    ":8080",
    Handler: otelhttp.Server(mux, otelhttp.WithServiceName("app-broker")),
}
```

```go
client := otelhttp.NewClient(otelhttp.WithTimeout(15 * time.Second))
resp, err := client.Get("https://api.example.com/v1/me")
```

`Server` and `NewClient` emit the standard `api_counter` and
`api_histogram` metrics on every non-excluded request (see "Standard
metrics" below). The upstream
`go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` is still
chained for trace context, but its semconv metrics are no longer the
primary signal.

#### Default exclusions

`Server` excludes `/healthz`, `/readyz`, and `/metrics` by default — these
paths do not appear in `api_counter` / `api_histogram`. Override via
`otelhttp.WithExcludedPaths(...)`.

### `metrics` — standard + adhoc metrics

Two tiers:

```go
// Standard tier — fixed names + label keys, caller fills option-driven values.
metrics.IncAPI(metrics.StatusSuccess,
    metrics.WithRoute("/v1/credential/oauth-user"),
    metrics.WithMethod("GET"),
    metrics.WithCode(200))

metrics.RecordAPILatency(elapsed, metrics.StatusFailure,
    metrics.WithRoute("/v1/charge"),
    metrics.WithMethod("POST"),
    metrics.WithCode(502))

// Adhoc tier — caller picks the name. Use sparingly.
_ = metrics.IncCounter("widget_processed", map[string]string{"kind": "premium"})
_ = metrics.RecordHistogram("widget_size_bytes", 4096, map[string]string{"kind": "premium"})
```

#### Status enum

| Constant            | Meaning                                                  |
|---------------------|----------------------------------------------------------|
| `StatusSuccess`     | Happy path (2xx, intended outcome).                      |
| `StatusError`       | Expected failure — 4xx, validation, consent revoked.     |
| `StatusFailure`     | Unexpected failure — 5xx, panic, dependency down.        |

#### Standard label set

Auto-attached to every standard AND adhoc metric:

| Label       | Source                                                |
|-------------|-------------------------------------------------------|
| `service`   | `Init.Config.ServiceName` (required)                  |
| `env`       | `Init.Config.Environment` (required)                  |
| `pod`       | env `HOSTNAME` or local hostname                      |
| `namespace` | env `POD_NAMESPACE` (k8s downward API)                |
| `host`      | env `NODE_NAME` or `EC2_INSTANCE_ID` or hostname      |
| `ip`        | env `POD_IP` or first non-loopback interface          |
| `cluster`   | `Init.Config.Cluster` or env `CLUSTER_NAME`           |
| `region`    | `Init.Config.Region` or env `REGION`                  |

Anything in `Init.Config.StaticLabels` overrides the auto-detected entry
of the same key. Inspect the active label set in tests via
`metrics.StandardLabels()`.

#### Adhoc names

Names starting with `api_` are reserved for the standard tier — adhoc
emitters return `metrics.ErrReservedName` if used. This keeps the
`api_*` namespace clean for cross-service dashboards.

### `health` — /healthz and /readyz

```go
mux.Handle("/healthz", health.Handler(serviceName, version, commit))
mux.Handle("/readyz", health.ReadinessHandler(serviceName, version, commit,
    health.Check{Name: "postgres", Fn: func(ctx context.Context) error {
        return db.PingContext(ctx)
    }},
    health.Check{Name: "vault", Fn: func(ctx context.Context) error {
        return vault.Ping(ctx)
    }},
))
```

`/healthz` always returns 200 with the service identity:

```json
{"service":"app-broker","version":"1.4.2","commit":"abc1234","status":"ok"}
```

`/readyz` runs every check on each request. All passing → 200 +
`status:"ok"`. Any failing → 503 + `status:"unhealthy"` + `reasons` array.

Each `Check.Timeout` defaults to 2s when zero. Both endpoints are
already in `otelhttp.Server`'s default-excluded-paths, so they are not
counted in the API metrics.

## Standard metrics emitted

| Name            | Kind          | Labels                                               | Source                            |
|-----------------|---------------|------------------------------------------------------|-----------------------------------|
| `api_counter`   | Counter       | `status`, `route`, `method`, `code` + standard set   | `otelhttp.Server` / `NewClient`   |
| `api_histogram` | Histogram (s) | `status`, `route`, `method`, `code` + standard set   | `otelhttp.Server` / `NewClient`   |

## What's NOT in the library

- **`version` is intentionally NOT a standard metric label.** Embedding the
  version in a label resets the time series on every deploy and breaks
  Grafana dashboard continuity. Service version belongs in `/healthz`
  and on trace resource attributes — never as a Prometheus label.
- **No span emission** in v0.x. Metrics-only. A span hook will land in a
  later release without changing the public surface.
- **No retry / circuit-breaker logic** in `otelhttp.NewClient`. That's a
  service concern; this library only enforces a default timeout.
- **No log shipper.** Log handling is left to the consuming service —
  stdout + a cluster-level collector is the recommended pattern.

## Versioning

Tag-based — consumers pin via `go get .../otelhttp@v0.2.0`. Each package
under this module shares the module-wide tag.

**Pre-1.0.** Breaking changes possible at minor bumps. v1.0.0 is tagged
once the API is stable across at least three consumers.

## Upgrading from v0.1.0

- `otelinit.Config.Environment` is now required. Set it explicitly.
- Metric names emitted by `otelhttp.Server` and `NewClient` changed from
  `http.server.request.duration` / `http.client.request.duration` to
  `api_histogram` + `api_counter`. Update Prometheus scrape configs and
  Grafana dashboards.
- `/healthz` and `/readyz` are no longer instrumented (they were also
  excluded in v0.1.0 — explicit confirmation in v0.2.0).
- New: import `github.com/treyamrich/golang-common/health` and serve
  `health.Handler(name, version, commit)` at `/healthz`.

## License

MIT — see [LICENSE](./LICENSE).
