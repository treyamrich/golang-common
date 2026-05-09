package otelhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/treyamrich/golang-common/log"
)

func TestServer_GeneratesCxidWhenMissing(t *testing.T) {
	_, cleanup := installManualReader(t)
	defer cleanup()

	var seen string
	wrapped := Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = log.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	if !log.IsValidCxid(seen) {
		t.Fatalf("handler did not see a valid cxid in ctx: %q", seen)
	}
	if got := resp.Header.Get(CorrelationIDHeader); got != seen {
		t.Fatalf("response header cxid mismatch: hdr=%q ctx=%q", got, seen)
	}
}

func TestServer_PreservesValidInboundCxid(t *testing.T) {
	_, cleanup := installManualReader(t)
	defer cleanup()

	want := "deadbeefcafebabe0123456789abcdef"
	var seen string
	wrapped := Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = log.FromContext(r.Context())
	}))
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/x", nil)
	req.Header.Set(CorrelationIDHeader, want)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	if seen != want {
		t.Fatalf("cxid not preserved: got %q want %q", seen, want)
	}
	if got := resp.Header.Get(CorrelationIDHeader); got != want {
		t.Fatalf("response header mismatch: %q", got)
	}
}

func TestServer_RejectsMalformedCxid(t *testing.T) {
	_, cleanup := installManualReader(t)
	defer cleanup()

	bad := "not-a-valid-cxid"
	var seen string
	wrapped := Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = log.FromContext(r.Context())
	}))
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/x", nil)
	req.Header.Set(CorrelationIDHeader, bad)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	if seen == bad {
		t.Fatalf("malformed cxid was not rejected")
	}
	if !log.IsValidCxid(seen) {
		t.Fatalf("expected a freshly minted cxid, got %q", seen)
	}
}

func TestServer_CxidPresentEvenForExcludedPaths(t *testing.T) {
	_, cleanup := installManualReader(t)
	defer cleanup()

	var seen string
	wrapped := Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = log.FromContext(r.Context())
	}))
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if !log.IsValidCxid(seen) {
		t.Fatalf("excluded path lost cxid: %q", seen)
	}
}

func TestClient_ForwardsCxidFromContext(t *testing.T) {
	_, cleanup := installManualReader(t)
	defer cleanup()

	want := "0123456789abcdef0123456789abcdef"
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(CorrelationIDHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	client := NewClient()
	req, _ := http.NewRequestWithContext(log.WithCxid(context.Background(), want), "GET", upstream.URL+"/x", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	if got != want {
		t.Fatalf("upstream did not see forwarded cxid: got %q want %q", got, want)
	}
}

func TestClient_DoesNotOverrideExplicitCxid(t *testing.T) {
	_, cleanup := installManualReader(t)
	defer cleanup()

	caller := "ffffffffffffffffffffffffffffffff"
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(CorrelationIDHeader)
	}))
	defer upstream.Close()

	client := NewClient()
	ctx := log.WithCxid(context.Background(), "00000000000000000000000000000000")
	req, _ := http.NewRequestWithContext(ctx, "GET", upstream.URL+"/x", nil)
	req.Header.Set(CorrelationIDHeader, caller)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	if got != caller {
		t.Fatalf("explicit caller header was overridden: got %q want %q", got, caller)
	}
}

func TestClient_NoCxidInContext_NoHeaderAdded(t *testing.T) {
	_, cleanup := installManualReader(t)
	defer cleanup()

	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(CorrelationIDHeader)
	}))
	defer upstream.Close()

	client := NewClient()
	resp, err := client.Get(upstream.URL + "/x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	if got != "" {
		t.Fatalf("expected no cxid header, got %q", got)
	}
}

// TestMetrics_DoNotIncludeCxidLabel asserts the cardinality discipline:
// even with a cxid in context AND a header in flight, the emitted
// api_counter / api_histogram MUST NOT carry cxid as a label.
func TestMetrics_DoNotIncludeCxidLabel(t *testing.T) {
	reader, cleanup := installManualReader(t)
	defer cleanup()

	cxid := log.NewCxid()
	wrapped := Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/x", nil)
	req.Header.Set(CorrelationIDHeader, cxid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					assertNoCxidLabel(t, m.Name, dp.Attributes.ToSlice(), cxid)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					assertNoCxidLabel(t, m.Name, dp.Attributes.ToSlice(), cxid)
				}
			}
		}
	}
}

func assertNoCxidLabel(t *testing.T, metricName string, attrs []attribute.KeyValue, cxid string) {
	t.Helper()
	for _, a := range attrs {
		k := string(a.Key)
		v := a.Value.AsString()
		if k == "cxid" {
			t.Errorf("metric %q has cxid label — would explode cardinality", metricName)
		}
		if v == cxid {
			t.Errorf("metric %q has a label whose value is the cxid (key=%q)", metricName, k)
		}
	}
}
