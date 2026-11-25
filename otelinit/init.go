// Package otelinit provides idiomatic OpenTelemetry SDK initialization for
// Go services consuming this library.
//
// Init wires the global MeterProvider and Propagators with either a
// Prometheus exporter (served by Handler, scraped from /metrics; the
// default, since the homelab runs Prometheus and no OTLP collector) or an
// OTLP gRPC push exporter, AND configures the metrics package's standard
// label set so every metric emits with the runtime context attached. Tracing is intentionally deferred — the
// same Init can be extended to enable a TracerProvider later without
// changing the public surface.
package otelinit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/treyamrich/golang-common/metrics"
)

// ErrAlreadyInitialized is returned when Init is called more than once in a
// single process. Callers can ignore it in tests where multiple Init calls
// across packages are common.
var ErrAlreadyInitialized = errors.New("otelinit: already initialized")

// Config controls Init behavior.
type Config struct {
	// ServiceName is required, e.g. "app-broker".
	ServiceName string

	// ServiceVersion is optional; "" defaults to "dev". Used for resource
	// attributes and (separately) /healthz — NOT as a metric label.
	ServiceVersion string

	// Environment is required. Examples: "prod", "staging", "dev".
	// Becomes the env standard metric label.
	Environment string

	// Cluster is optional. Example: "prod-us-east".
	// Falls back to env CLUSTER_NAME.
	Cluster string

	// Region is optional. Example: "us-east-1".
	// Falls back to env REGION.
	Region string

	// StaticLabels is optional and attaches extra labels to every metric.
	// Values here override any auto-detected entry with the same key.
	StaticLabels map[string]string

	// Exporter is "prometheus" or "otlp". Empty means otlp when an OTLP
	// endpoint is configured, prometheus otherwise.
	Exporter string

	// OTLPEndpoint is the OTLP gRPC endpoint. Falls back to
	// OTEL_EXPORTER_OTLP_ENDPOINT. Required for the otlp exporter.
	OTLPEndpoint string

	// Insecure skips TLS to the collector — typical when the collector is
	// reachable via in-cluster service DNS.
	Insecure bool

	// PropagatorNames selects W3C propagators to install on the global
	// TextMapPropagator. Default: ["tracecontext", "baggage"].
	PropagatorNames []string
}

var (
	initMu   sync.Mutex
	initDone bool
)

// Init wires the global MeterProvider, Propagators, and metrics standard
// label set per cfg. It is idempotent within a process — subsequent calls
// return ErrAlreadyInitialized.
//
// The returned shutdown should be deferred in main. It flushes pending
// metrics with a 5s timeout (overridable via the ctx passed to shutdown).
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	initMu.Lock()
	defer initMu.Unlock()

	if initDone {
		return func(context.Context) error { return nil }, ErrAlreadyInitialized
	}

	if cfg.ServiceName == "" {
		return nil, errors.New("otelinit: ServiceName is required")
	}
	if cfg.Environment == "" {
		return nil, errors.New("otelinit: Environment is required")
	}
	endpoint := cfg.OTLPEndpoint
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	exp := cfg.Exporter
	if exp == "" {
		exp = "prometheus"
		if endpoint != "" {
			exp = "otlp"
		}
	}
	if exp == "otlp" && endpoint == "" {
		return nil, errors.New("otelinit: OTLPEndpoint or OTEL_EXPORTER_OTLP_ENDPOINT is required for the otlp exporter")
	}
	if exp != "otlp" && exp != "prometheus" {
		return nil, fmt.Errorf("otelinit: unknown exporter %q", exp)
	}
	version := cfg.ServiceVersion
	if version == "" {
		version = "dev"
	}

	hostname, _ := os.Hostname()
	if envHost := os.Getenv("HOSTNAME"); envHost != "" {
		hostname = envHost
	}

	res, err := resource.Merge(
		resource.Default(),
		// Schemaless: resource.Default() carries the SDK's own semconv
		// schema URL, which changes with SDK upgrades; merging two
		// different URLs is an error.
		resource.NewSchemaless(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(version),
			semconv.ServiceInstanceID(uuid.NewString()),
			semconv.HostName(hostname),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otelinit: resource: %w", err)
	}

	var reader sdkmetric.Reader
	if exp == "otlp" {
		expOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(endpoint)}
		if cfg.Insecure {
			expOpts = append(expOpts, otlpmetricgrpc.WithInsecure())
		}
		exporter, err := otlpmetricgrpc.New(ctx, expOpts...)
		if err != nil {
			return nil, fmt.Errorf("otelinit: otlp exporter: %w", err)
		}
		reader = sdkmetric.NewPeriodicReader(exporter)
		promHandler.Store(nil)
	} else {
		reg := prometheus.NewRegistry()
		// Names stay exactly as emitted (api_histogram, not
		// api_histogram_seconds), and no otel_scope_* labels.
		pe, err := otelprom.New(otelprom.WithRegisterer(reg),
			otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithoutSuffixes),
			otelprom.WithoutScopeInfo(), otelprom.WithoutTargetInfo())
		if err != nil {
			return nil, fmt.Errorf("otelinit: prometheus exporter: %w", err)
		}
		reader = pe
		h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
		promHandler.Store(&h)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
	)
	otel.SetMeterProvider(mp)

	otel.SetTextMapPropagator(buildPropagator(cfg.PropagatorNames))

	if err := metrics.Configure(metrics.Config{
		ServiceName:  cfg.ServiceName,
		Environment:  cfg.Environment,
		Cluster:      cfg.Cluster,
		Region:       cfg.Region,
		StaticLabels: cfg.StaticLabels,
	}); err != nil {
		return nil, fmt.Errorf("otelinit: metrics configure: %w", err)
	}

	initDone = true

	shutdown = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		initMu.Lock()
		initDone = false
		initMu.Unlock()
		return mp.Shutdown(ctx)
	}
	return shutdown, nil
}

var promHandler atomic.Pointer[http.Handler]

// Handler serves the Prometheus exposition of every metric when Init
// chose the prometheus exporter, and 404 otherwise. Mount it at /metrics.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := promHandler.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
}

func buildPropagator(names []string) propagation.TextMapPropagator {
	if len(names) == 0 {
		names = []string{"tracecontext", "baggage"}
	}
	props := make([]propagation.TextMapPropagator, 0, len(names))
	for _, n := range names {
		switch n {
		case "tracecontext":
			props = append(props, propagation.TraceContext{})
		case "baggage":
			props = append(props, propagation.Baggage{})
		}
	}
	return propagation.NewCompositeTextMapPropagator(props...)
}
