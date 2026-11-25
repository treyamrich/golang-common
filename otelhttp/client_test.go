package otelhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/treyamrich/golang-common/internal/metrictest"
	"github.com/treyamrich/golang-common/log"
)

func TestNewClient_DefaultTimeout(t *testing.T) {
	if c := NewClient(); c.Timeout != DefaultClientTimeout {
		t.Fatalf("timeout=%v", c.Timeout)
	}
	if c := NewClient(WithTimeout(time.Second)); c.Timeout != time.Second {
		t.Fatalf("timeout=%v", c.Timeout)
	}
}

func TestNewClient_RecordsDependencyMetrics(t *testing.T) {
	r := metrictest.Install(t)
	code := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
	defer srv.Close()
	c := NewClient(WithDependency("spotify"))

	for _, code = range []int{200, 404, 503, 429} {
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("want error")
	}

	got := r.Counts(t, "dependency_histogram", "status")
	for k, v := range map[string]uint64{"success": 1, "error": 1, "failure": 2, "canceled": 1} {
		if got[k] != v {
			t.Errorf("%s: got %d want %d (%v)", k, got[k], v, got)
		}
	}
	if deps := r.Counts(t, "dependency_histogram", "dependency"); deps["spotify"] != 5 {
		t.Errorf("dependency label: %v", deps)
	}
	if ops := r.Counts(t, "dependency_histogram", "operation"); ops["GET"] != 5 {
		t.Errorf("operation label: %v", ops)
	}
}

func TestClient_ForwardsCxid(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(CorrelationIDHeader))
	}))
	defer srv.Close()
	c := NewClient()
	cxid := log.NewCxid()

	req, _ := http.NewRequestWithContext(log.WithCxid(context.Background(), cxid), http.MethodGet, srv.URL, nil)
	resp, _ := c.Do(req)
	resp.Body.Close()

	req, _ = http.NewRequestWithContext(log.WithCxid(context.Background(), cxid), http.MethodGet, srv.URL, nil)
	req.Header.Set(CorrelationIDHeader, "explicit")
	resp, _ = c.Do(req)
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, _ = c.Do(req)
	resp.Body.Close()

	if got[0] != cxid || got[1] != "explicit" || got[2] != "" {
		t.Fatalf("got %q", got)
	}
}
