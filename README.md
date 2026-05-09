# golang-common

Shared Go libraries for homelab services. v0.1.0 ships an OpenTelemetry
metrics baseline: HTTP server middleware, HTTP client wrapper, and an init
helper. Tracing is intentionally deferred to a later release.

## Install

```sh
go get github.com/treyamrich/golang-common/otelinit@v0.1.0
go get github.com/treyamrich/golang-common/otelhttp@v0.1.0
```

## Packages

### `otelinit` — one-shot OTEL SDK setup

```go
import (
    "context"
    "log"
    "os"

    "github.com/treyamrich/golang-common/otelinit"
)

func main() {
    ctx := context.Background()
    shutdown, err := otelinit.Init(ctx, otelinit.Config{
        ServiceName:    "app-broker",
        ServiceVersion: version,
        OTLPEndpoint:   os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
        Insecure:       true,
    })
    if err != nil {
        log.Fatal(err)
    }
    defer shutdown(context.Background())

    // ... rest of main ...
}
```

`Init` wires the global `MeterProvider` with an OTLP gRPC exporter and the
W3C `tracecontext` + `baggage` propagators. Resource attributes attached:
`service.name`, `service.version`, `service.instance.id` (random UUID), and
`host.name` (from `HOSTNAME` env or os.Hostname). The returned `shutdown`
flushes pending metrics with a 5s timeout — defer it in `main`.

### `otelhttp` — HTTP server middleware + client wrapper

```go
import (
    "net/http"

    "github.com/treyamrich/golang-common/otelhttp"
)

mux := http.NewServeMux()
mux.HandleFunc("/v1/credential/oauth-user", handleOAuthUser)

server := &http.Server{
    Addr:    ":8080",
    Handler: otelhttp.Server(mux, otelhttp.WithServiceName("app-broker")),
}
```

```go
client := otelhttp.NewClient(otelhttp.WithTimeout(15 * time.Second))
resp, err := client.Get("https://api.spotify.com/v1/me")
// metrics for this call land in http.client.* with labels.
```

These are thin wrappers around
`go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`. Our value:
sane defaults (path excludes, mandatory client timeout, service-name
plumbing) so each service does not re-derive them.

#### Default exclusions

`Server` excludes `/healthz`, `/readyz`, and `/metrics` by default. Override
via `otelhttp.WithExcludedPaths(...)`.

## Metrics emitted

| Name                            | Kind          | Labels                                                                    | Source           |
|---------------------------------|---------------|---------------------------------------------------------------------------|------------------|
| `http.server.request.duration`  | Histogram (s) | `http.request.method`, `http.response.status_code`, `http.route`, `server.address` | `Server`         |
| `http.server.active_requests`   | UpDownCounter | `http.request.method`, `server.address`                                  | `Server`         |
| `http.client.request.duration`  | Histogram (s) | `http.request.method`, `http.response.status_code`, `server.address`     | `NewClient`      |
| `http.client.request.body.size` | Counter       | `http.request.method`, `server.address`                                  | `NewClient`      |

Names and labels follow the upstream OTEL HTTP semantic conventions; the
exact set is what the upstream `contrib/instrumentation/net/http/otelhttp`
package emits. Use these as the basis for Prometheus / Grafana panels.

## Versioning

Tag-based — consumers pin via `go get .../otelhttp@v0.1.0`. Each package
under this module shares the module-wide tag.

## Stability

**Pre-1.0.** Breaking changes possible at minor bumps. v1.0.0 is tagged once
the API is stable across at least three consumers.

## License

MIT — see [LICENSE](./LICENSE).
