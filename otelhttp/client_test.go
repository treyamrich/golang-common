package otelhttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestNewClient_DefaultTimeout(t *testing.T) {
	c := NewClient()
	if c.Timeout != DefaultClientTimeout {
		t.Errorf("expected default timeout %v, got %v", DefaultClientTimeout, c.Timeout)
	}
	if c.Transport == nil {
		t.Fatal("transport should be set")
	}
}

func TestNewClient_WithTimeout(t *testing.T) {
	c := NewClient(WithTimeout(2 * time.Second))
	if c.Timeout != 2*time.Second {
		t.Errorf("expected 2s timeout, got %v", c.Timeout)
	}
}

func TestNewClient_WithBaseTransport(t *testing.T) {
	custom := &http.Transport{}
	c := NewClient(WithBaseTransport(custom))
	if c.Transport == nil {
		t.Fatal("transport should be set")
	}
}

func TestNewClient_RecordsMetrics(t *testing.T) {
	reader, cleanup := installManualReader(t)
	defer cleanup()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	client := NewClient()
	resp, err := client.Get(srv.URL + "/test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "http.client.request.duration" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected http.client.request.duration to be recorded")
	}
}

func TestNewClient_WithUpstreamOptions(t *testing.T) {
	c := NewClient(WithClientUpstreamOptions())
	if c.Transport == nil {
		t.Fatal("transport should be set")
	}
}
