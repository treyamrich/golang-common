package otelinit

import (
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// resetForTest clears process-global init state. Test-only.
func resetForTest() {
	initMu.Lock()
	initDone = false
	initMu.Unlock()
}

func TestInit_RequiresServiceName(t *testing.T) {
	resetForTest()
	_, err := Init(context.Background(), Config{OTLPEndpoint: "localhost:4317", Insecure: true})
	if err == nil {
		t.Fatal("expected error for missing ServiceName")
	}
}

func TestInit_RequiresEndpoint(t *testing.T) {
	resetForTest()
	os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	_, err := Init(context.Background(), Config{ServiceName: "svc"})
	if err == nil {
		t.Fatal("expected error for missing endpoint")
	}
}

func TestInit_FallsBackToEnvEndpoint(t *testing.T) {
	resetForTest()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	shutdown, err := Init(context.Background(), Config{ServiceName: "svc", Insecure: true})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer shutdown(context.Background())
}

func TestInit_Idempotent(t *testing.T) {
	resetForTest()
	shutdown, err := Init(context.Background(), Config{
		ServiceName:  "svc",
		OTLPEndpoint: "localhost:4317",
		Insecure:     true,
	})
	if err != nil {
		t.Fatalf("first init: %v", err)
	}
	defer shutdown(context.Background())

	_, err2 := Init(context.Background(), Config{
		ServiceName:  "svc",
		OTLPEndpoint: "localhost:4317",
		Insecure:     true,
	})
	if err2 != ErrAlreadyInitialized {
		t.Fatalf("expected ErrAlreadyInitialized, got %v", err2)
	}
}

func TestInit_DefaultPropagators(t *testing.T) {
	p := buildPropagator(nil)
	fields := p.Fields()
	hasTraceparent := false
	hasBaggage := false
	for _, f := range fields {
		if f == "traceparent" {
			hasTraceparent = true
		}
		if f == "baggage" {
			hasBaggage = true
		}
	}
	if !hasTraceparent || !hasBaggage {
		t.Fatalf("expected traceparent + baggage propagators, got %v", fields)
	}
}

// TestInit_ManualReaderEndToEnd verifies that, when the global MeterProvider
// is exercised, exported metric data is observable via a ManualReader. We
// don't use Init() here (it requires a live OTLP collector); instead we
// install a MeterProvider with a ManualReader and confirm the upstream SDK
// path the package depends on works.
func TestInit_ManualReaderEndToEnd(t *testing.T) {
	resetForTest()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	defer mp.Shutdown(context.Background())

	meter := otel.Meter("test")
	ctr, err := meter.Int64Counter("test.counter")
	if err != nil {
		t.Fatalf("counter: %v", err)
	}
	ctr.Add(context.Background(), 1)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(rm.ScopeMetrics) == 0 {
		t.Fatal("expected scope metrics")
	}
}
