package otelhttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/treyamrich/golang-common/metrics"
)

// installManualReader installs a fresh MeterProvider with a ManualReader on
// the OTEL global and configures the metrics package. Returns the reader
// and a cleanup func.
func installManualReader(t *testing.T) (*sdkmetric.ManualReader, func()) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	if err := metrics.Configure(metrics.Config{ServiceName: "test", Environment: "test"}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	return reader, func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(prev)
	}
}

func collectMetricNames(t *testing.T, reader *sdkmetric.ManualReader) map[string]bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	return names
}

func TestServer_RecordsRequestMetrics(t *testing.T) {
	reader, cleanup := installManualReader(t)
	defer cleanup()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	wrapped := Server(handler, WithServiceName("test-svc"))

	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/foo")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	names := collectMetricNames(t, reader)
	if !names["api_histogram"] || !names["api_counter"] {
		t.Errorf("expected api_histogram + api_counter, got %v", names)
	}
}

func TestServer_DifferentStatusCodes(t *testing.T) {
	reader, cleanup := installManualReader(t)
	defer cleanup()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/notfound":
			w.WriteHeader(http.StatusNotFound)
		case "/err":
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	srv := httptest.NewServer(Server(handler))
	defer srv.Close()

	for _, p := range []string{"/ok", "/notfound", "/err"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("get %s: %v", p, err)
		}
		resp.Body.Close()
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	var dataPoints int
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "api_histogram" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			for _, dp := range h.DataPoints {
				dataPoints += int(dp.Count)
			}
		}
	}
	if dataPoints != 3 {
		t.Errorf("expected 3 histogram data points across statuses, got %d", dataPoints)
	}
}

func TestServer_ExcludesDefaultPaths(t *testing.T) {
	reader, cleanup := installManualReader(t)
	defer cleanup()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(Server(handler))
	defer srv.Close()

	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("get %s: %v", p, err)
		}
		resp.Body.Close()
	}

	names := collectMetricNames(t, reader)
	if names["api_histogram"] || names["api_counter"] {
		t.Errorf("expected default-excluded paths to NOT emit metrics, got %v", names)
	}
}

func TestServer_OverrideExcludedPaths(t *testing.T) {
	reader, cleanup := installManualReader(t)
	defer cleanup()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(Server(handler, WithExcludedPaths("/api/foo")))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	names := collectMetricNames(t, reader)
	if !names["api_histogram"] {
		t.Errorf("/healthz should be instrumented when not in excludes, got %v", names)
	}
}

func TestServer_WithOperationName(t *testing.T) {
	_, cleanup := installManualReader(t)
	defer cleanup()
	h := Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		WithOperationName("custom.op"))
	if h == nil {
		t.Fatal("nil handler")
	}
}

func TestExcludeFilter_EmptyAllowsAll(t *testing.T) {
	f := excludeFilter(nil)
	req := httptest.NewRequest("GET", "/healthz", nil)
	if !f(req) {
		t.Fatal("empty excludes should allow everything")
	}
}

func TestStatusString(t *testing.T) {
	if statusString(404) != "404" {
		t.Fatal("statusString")
	}
}

func TestClassify(t *testing.T) {
	if classify(200) != metrics.StatusSuccess {
		t.Fatal("200 should be success")
	}
	if classify(404) != metrics.StatusError {
		t.Fatal("404 should be error")
	}
	if classify(500) != metrics.StatusFailure {
		t.Fatal("500 should be failure")
	}
}
