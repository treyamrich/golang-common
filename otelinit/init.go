// Package otelinit provides idiomatic OpenTelemetry SDK initialization for
// homelab Go services.
//
// v0.1.0 wires the global MeterProvider and Propagators with an OTLP gRPC
// metric exporter. Tracing is intentionally deferred — the same Init can be
// extended to enable a TracerProvider later without changing the public
// surface.
package otelinit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// ErrAlreadyInitialized is returned when Init is called more than once in a
// single process. Callers can ignore it in tests where multiple Init calls
// across packages are common.
var ErrAlreadyInitialized = errors.New("otelinit: already initialized")

// Config controls Init behavior.
//
// Future-compatibility: TraceSampleRate, MetricExportInterval, and
// ResourceAttrs map[string]string can be added without breaking the API.
type Config struct {
	// ServiceName is required, e.g. "app-broker".
	ServiceName string

	// ServiceVersion is optional; "" defaults to "dev".
	ServiceVersion string

	// OTLPEndpoint is the OTLP gRPC endpoint, e.g.
	// "otel-collector.monitoring.svc:4317". If empty, falls back to the
	// OTEL_EXPORTER_OTLP_ENDPOINT environment variable. Required (one of
	// the two).
	OTLPEndpoint string

	// Insecure skips TLS to the collector — typical for in-cluster.
	Insecure bool

	// PropagatorNames selects W3C propagators to install on the global
	// TextMapPropagator. Default: ["tracecontext", "baggage"].
	// Recognized: "tracecontext", "baggage".
	PropagatorNames []string
}

var (
	initMu   sync.Mutex
	initDone bool
)

// Init wires the global MeterProvider and Propagators per cfg. It is
// idempotent within a process — subsequent calls return
// ErrAlreadyInitialized.
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
	endpoint := cfg.OTLPEndpoint
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if endpoint == "" {
		return nil, errors.New("otelinit: OTLPEndpoint or OTEL_EXPORTER_OTLP_ENDPOINT is required")
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
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(version),
			semconv.ServiceInstanceID(uuid.NewString()),
			semconv.HostName(hostname),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otelinit: resource: %w", err)
	}

	expOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(endpoint)}
	if cfg.Insecure {
		expOpts = append(expOpts, otlpmetricgrpc.WithInsecure())
	}
	exporter, err := otlpmetricgrpc.New(ctx, expOpts...)
	if err != nil {
		return nil, fmt.Errorf("otelinit: otlp exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	otel.SetMeterProvider(mp)

	otel.SetTextMapPropagator(buildPropagator(cfg.PropagatorNames))

	initDone = true

	shutdown = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		// Reset state so tests/process-replays can re-Init.
		initMu.Lock()
		initDone = false
		initMu.Unlock()
		return mp.Shutdown(ctx)
	}
	return shutdown, nil
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
