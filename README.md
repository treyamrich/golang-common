# golang-common

Shared Go libraries that make every homelab service look the same from
the outside:
- the same bootstrap and shutdown;
- the same HTTP middleware and error bodies;
- the same auth;
- the same metrics and logs.

A service built on it differs only in its core logic, dependencies and
routes. [go-service-template](https://github.com/treyamrich/go-service-template)
is the reference service; start new services from it.

## Install

```sh
go get github.com/treyamrich/golang-common@v0.5.0
```

All packages share the module version.

## Packages

| Package | What it gives a service |
|---|---|
| `app` | `app.Run(name, setup)`: config, logging and metrics init, admin server (`/healthz` `/readyz` `/metrics`), dependency start/close, runners, job loops, graceful shutdown |
| `config` | `config.Load[T]()`: env vars into a struct (`env:` tags, defaults, `required`, `file`) |
| `api` | HTTP server + standard middleware chain: correlation ID, `api_histogram` + access log, client-disconnect → 499, panic recovery, timeout, body limit; error handlers for oapi-codegen's strict server |
| `auth` | Bearer JWT verification against OIDC issuers (Authentik), `Principal` on the context, `auth.OpenAPI` (per-operation, enforces the spec's `security`), `auth.Middleware`, `auth.RequireScopes` |
| `auth/authtest` | A fake issuer that mints tokens, for handler tests |
| `apperr` | Error kinds shared by core and adapters → HTTP status, metrics status, standard error body |
| `observe` | `observe.Call` / `observe.Do`: wrap any dependency call → `dependency_histogram` |
| `postgres` | pgx pool as an `app` dependency (Start pings with retry, Check for readiness, Close) |
| `otelhttp` | `NewClient`: outbound HTTP with cxid forwarding + `dependency_histogram` |
| `otelinit` | MeterProvider setup: Prometheus (default) or OTLP |
| `metrics` | The three standard histograms + adhoc metrics |
| `log` | JSON slog, cxid on every `*Context` log call |
| `health` | `/healthz` and `/readyz` handlers (used by `app`) |
| `appbroker` | app-broker client SDK |

### `app` — process bootstrap

```go
type Config struct {
    API  api.Config      // HTTP_ADDR, HTTP_REQUEST_TIMEOUT, ...
    DB   postgres.Config // DB_HOST, DB_USER, ...
    Auth auth.OIDCConfig // AUTH_ISSUERS, AUTH_AUDIENCES
}

func main() {
    app.Run("tasks-api", func(ctx context.Context, a *app.App, cfg Config) error {
        db, err := postgres.New(cfg.DB)
        if err != nil {
            return err
        }
        if err := a.Start(ctx, "postgres", db); err != nil { // pinged now, closed last
            return err
        }
        verifier, err := auth.NewOIDC(cfg.Auth)
        if err != nil {
            return err
        }
        svc := core.NewService(pgstore.NewStore(db.Pool))
        a.Add(api.New("api", cfg.API, httpin.NewHandler(svc, verifier)))
        return nil
    })
}
```

A worker is the same without routes:

```go
a.Every("complete-pending", 10*time.Second, job) // fresh cxid + job_histogram per run, panics survive
a.Once("nightly-sync", job)                      // CronJob: process exits when done, 1 on error
a.Go("consumer", func(ctx context.Context) error { ... }) // anything long-running
```

Lifecycle:

1. Load the base config (`APP_ENV`, `ADMIN_ADDR`, `SHUTDOWN_*`) and the service's config. An error exits 1.
2. Init logs and metrics.
3. Start the admin server on `:8081`. `/readyz` returns 503 until setup returns, then runs each started dependency's `Check`.
4. Run setup.
5. Run the runners until SIGTERM, a runner error, or all runners have finished.
6. On shutdown:
   - `/readyz` → 503;
   - sleep `SHUTDOWN_DELAY` (5s);
   - cancel the runners and wait for them;
   - close dependencies in reverse order;
   - flush metrics.

   It is bounded by `SHUTDOWN_TIMEOUT` (25s).

### `api` — inbound HTTP

`api.New(name, cfg, handler)` serves `handler` behind this chain, outermost first:

1. **CorrelationID.**
2. **Observe:** `api_histogram` plus one access-log line. A request whose client went away is recorded as **499 / `canceled`**, never a 5xx.
3. **Recover.**
4. **Timeout.**
5. **LimitBody.**

The `api` label is the router's matched pattern, e.g. `get_/v1/tasks/{id}`. It is bounded by construction, and set for every response, including auth rejections. Unrouted requests are `unmatched`. Use `api.Route(name, h)` to override it.

With oapi-codegen (`std-http-server` + `strict-server`):

```go
strict := openapi.NewStrictHandlerWithOptions(srv, nil, openapi.StrictHTTPServerOptions{
    RequestErrorHandlerFunc:  api.RequestError,  // unparsable request → 400
    ResponseErrorHandlerFunc: api.ResponseError, // returned error → its apperr status + body
})
h := openapi.HandlerWithOptions(strict, openapi.StdHTTPServerOptions{
    BaseRouter:       http.NewServeMux(),
    Middlewares:      []openapi.MiddlewareFunc{auth.OpenAPI(verifier, openapi.OidcScopes)},
    ErrorHandlerFunc: api.RequestError,
})
```

### `auth` — authentication and scopes

- **`auth.NewOIDC(cfg)`** verifies tokens:
  - RS256, from a configured issuer, checked against its JWKS;
  - unexpired;
  - an accepted audience.

  Discovery is lazy, so an IdP outage at startup doesn't keep the service down; affected requests get 503. Keys refresh hourly and on an unknown `kid`. Scopes come from `scope` (Authentik) or `scp`.
- **`auth.OpenAPI(v, openapi.OidcScopes)`** enforces each operation's `security` from the spec:
  - required: authenticate, then require the listed scopes;
  - `security: []`: public.
- **`auth.Middleware(v, scopes...)`** protects a whole handler.
- **`auth.RequireScopes(ctx, ...)`** returns `apperr` Unauthenticated or Forbidden.
- **`auth.FromContext(ctx)`** returns the `Principal`.
- **Responses:** 401 (with `WWW-Authenticate`) or 403 in the standard error body. The specific reason is logged, never returned.

### `apperr` — error kinds

| Kind | HTTP | metrics status |
|---|---|---|
| `Invalid` | 400 | error |
| `Unauthenticated` | 401 | error |
| `Forbidden` | 403 | error |
| `NotFound` | 404 | error |
| `Conflict` | 409 | error |
| `Unavailable` | 503 | failure |
| any other error | 500 (generic message) | failure |
| unclassified `context.Canceled` | 499 | canceled |
| unclassified `context.DeadlineExceeded` | 504 | failure |

`apperr.WriteHTTP` writes `{"error":{"code","message","correlation_id"}}`.

### `observe` — dependency metrics

```go
task, err := observe.Call(ctx, "postgres", "GetTask", func(ctx context.Context) (core.Task, error) {
    return s.get(ctx, id)
})
err := observe.Do(ctx, "redis", "Set", func(ctx context.Context) error { ... })
```

Each call records one `dependency_histogram{dependency, operation, status}` sample.
- **Default status:** `apperr.Status`. Our own cancelled context is `canceled`; a dependency that returns "canceled" by itself is `failure`.
- **Override** with a classifier: `func(res T, err error) metrics.Status`.
- **Panics** are recorded as `failure` and re-panicked.
- **Labels** must be constants.

### `otelinit` and `metrics`

`app` calls `otelinit.Init`. The default exporter is Prometheus, served by `otelinit.Handler()` at the admin `/metrics`. Setting `OTEL_EXPORTER_OTLP_ENDPOINT` switches to OTLP push.

Standard metrics:

| Name | Labels (+ standard set) | Recorded by |
|---|---|---|
| `api_histogram` | `api`, `http_status`, `status` | `api` |
| `dependency_histogram` | `dependency`, `operation`, `status` | `observe`, `otelhttp.NewClient` |
| `job_histogram` | `job`, `status` | `app.Every` / `Once` / `RunJob` |

- **Units and counts.** All three are in seconds, and each `_count` is the call counter.
- **The standard set:** `service`, `env`, `pod`, `namespace`, `host`, `ip`, `cluster`, `region`. Scrape with `honorLabels: true` so it isn't renamed to `exported_*`.
- **`status` values:** `success`, `error`, `failure`, `canceled`.
- **Adhoc metrics:** `metrics.IncCounter` / `RecordHistogram`. The prefixes `api_`, `dependency_` and `job_` are reserved.

### `otelhttp` — outbound HTTP

```go
client := otelhttp.NewClient(otelhttp.WithDependency("spotify")) // 10s default timeout
```

- **Correlation ID:** forwards the context's cxid as `X-Correlation-Id`.
- **Metrics:** records `dependency_histogram{dependency, operation=<method>}`.
- **Status:** a dependency's 5xx or 429 is `failure`, another 4xx is `error`, and our own cancellation is `canceled`.

### `log` — structured logging + correlation IDs

```go
if err := log.Init(log.Config{
    Level:       os.Getenv("LOG_LEVEL"),       // debug|info|warn|error, default info
    StaticAttrs: map[string]string{"service": "app-broker", "env": "prod"},
}); err != nil && !errors.Is(err, log.ErrAlreadyInitialized) {
    panic(err)
}

// In a request handler:
func handle(w http.ResponseWriter, r *http.Request) {
    logger := log.New(r.Context())
    logger.Info("processing request", "user_id", userID)
    // {"time":"...","level":"INFO","msg":"processing request",
    //  "cxid":"<32 hex>","user_id":"...","service":"app-broker","env":"prod"}
}
```

`Init` configures `slog.Default` to write JSON to stderr with RFC3339Nano
timestamps. It is idempotent — second and later calls return
`log.ErrAlreadyInitialized` (callers in tests can ignore it). Bad
`LOG_LEVEL` values are coerced to `info` with a one-time warning rather
than panicking.

Every `slog.*Context(ctx, ...)` call carries the context's cxid (the
handler `Init` installs reads it). `log.New(ctx)` returns a logger
pre-bound with it, for code that logs without a context.

### Correlation IDs

A correlation ID (`cxid`) is a 32-hex-char UUIDv4 (no hyphens) that ties a
single request's logs together across services. The flow is fully
automatic when both ends use this library:

1. **Inbound:** the `api` package reads the `X-Correlation-Id` header. If
   it matches `^[a-f0-9]{32}$` it's reused; otherwise a fresh one is
   generated. The cxid is injected into `r.Context()` via `log.WithCxid`
   and echoed back on the response as `X-Correlation-Id` (so the caller
   can grep their own logs).
2. **In-process:** every `slog.*Context(ctx, ...)` line carries it.
   Worker job runs (`app.Every` / `RunJob`) mint one per run.
3. **Outbound:** `otelhttp.NewClient` reads `log.FromContext(req.Context())`
   on every outbound call and sets `X-Correlation-Id` on the wire (only
   if the caller didn't already set the header). The next service's
   middleware extracts the same cxid → its logs share it. End-to-end
   correlation is one-import deep.

The cxid is intentionally NOT a metric label (see "What's NOT").

### `appbroker` — app-broker client SDK

Third-party API auth for homelab services goes through app-broker: it
holds provider credentials, runs every exchange and refresh, and hands
services short-lived access tokens. This package is the typed client.
Standard library only.

A service declares what it uses in its own chart (app-template's
`appBroker:` block renders a `BrokerConsumer`): `appCapabilities` for
app-level apps (client credentials, granted at deploy) and `userApps`
for per-user apps (usable only for users who installed them). The same
block mounts a projected service-account token, which `FromEnv` reads:

```go
c, err := appbroker.FromEnv() // APP_BROKER_URL, APP_BROKER_TOKEN_FILE (or APP_BROKER_TOKEN off-cluster)
tokens := appbroker.NewTokenCache(c)

// App-level capability.
tok, err := tokens.ClientToken(ctx, "spotify-catalog", "")

// Per-user: act for exactly the users who installed the app.
err = c.ForEachInstall(ctx, "spotify-playlist-bot", func(ctx context.Context, in appbroker.Install) error {
    tok, err := tokens.UserToken(ctx, in)
    if errors.Is(err, appbroker.ErrConsentRevoked) || errors.Is(err, appbroker.ErrNotFound) {
        return nil // user must reinstall from the dashboard
    }
    if err != nil {
        return err
    }
    return run(ctx, in, tok.AccessToken)
})
```

A UI acting for the signed-in user forwards their Authentik ID token
with `appbroker.WithIDToken(ctx, idToken)`. Install, uninstall and
listing a user's connections require it.

Errors are typed sentinels (`ErrForbidden`, `ErrNotFound`,
`ErrConsentRevoked`, `ErrUpstreamUnavailable`, …) usable with
`errors.Is`.

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

Each `Check.Timeout` defaults to 2s when zero. `app` serves both on the
admin port, so they never reach the API metrics.

## What's NOT in the library

- **`version` is intentionally NOT a standard metric label.** Embedding the
  version in a label resets the time series on every deploy and breaks
  Grafana dashboard continuity. Service version belongs in `/healthz`
  and on trace resource attributes — never as a Prometheus label.
- **No span emission** in v0.x. Metrics-only. A span hook will land in a
  later release without changing the public surface.
- **No retry / circuit-breaker logic** in `otelhttp.NewClient`. That's a
  service concern; this library only enforces a default timeout.
- **No log shipper.** The `log` package emits structured JSON to stderr;
  shipping to a cluster-level collector (Loki, ELK, etc.) is the
  consuming service's concern.
- **`cxid` is intentionally NOT a metric label.** A per-request label
  would explode time-series cardinality. Cxid belongs on logs and traces
  — never on metric series.
- **No trace ID propagation in the `log` package.** When span emission
  lands in a later release the request-scope logger will gain a
  `trace_id` attribute alongside `cxid`. Until then, the cxid is the
  primary correlation primitive.

## Versioning

Tag-based — consumers pin via `go get .../otelhttp@v0.2.0`. Each package
under this module shares the module-wide tag.

**Pre-1.0.** Breaking changes possible at minor bumps. v1.0.0 is tagged
once the API is stable across at least three consumers.

## Upgrading from v0.4.0

v0.5.0 breaks the HTTP and metrics APIs:

- **`otelhttp.Server` is gone.** Serve through `api.New` / `api.Chain`, or better, `app.Run`.
- **Metric changes:**
  - `metrics.IncAPI`, `RecordAPILatency` and `With*` are replaced by `RecordAPI`, `RecordDependency` and `RecordJob`;
  - `api_counter` is gone: use `api_histogram_count`;
  - `api_histogram`'s labels are now `api`, `http_status` and `status`.
- **Outbound calls** record `dependency_histogram`, not `api_*`.
- **The default exporter is now Prometheus.** It is OTLP only when an endpoint is set.

## Upgrading from v0.2.0

v0.3.0 is fully backward-compatible. Cxid features are opt-in by
importing the new `log/` package; existing v0.2.0 callers see no
behavioural change beyond the addition of an `X-Correlation-Id` header
on `otelhttp.Server` responses (and auto-forwarding from
`otelhttp.NewClient` when the caller's context carries one).

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
