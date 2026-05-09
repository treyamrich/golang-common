package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func installReader(t *testing.T) (*sdkmetric.ManualReader, func()) {
	t.Helper()
	r := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	return r, func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(prev)
		resetForTest()
	}
}

func mustConfigure(t *testing.T, extra map[string]string) {
	t.Helper()
	cfg := Config{
		ServiceName:  "svc",
		Environment:  "test",
		Cluster:      "c1",
		Region:       "us-east-1",
		StaticLabels: extra,
	}
	if err := Configure(cfg); err != nil {
		t.Fatalf("configure: %v", err)
	}
}

func collectByName(t *testing.T, r *sdkmetric.ManualReader, name string) (metricdata.Metrics, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func attrSet(kvs []attribute.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func histAttrs(m metricdata.Metrics) map[string]string {
	h, ok := m.Data.(metricdata.Histogram[float64])
	if !ok || len(h.DataPoints) == 0 {
		return nil
	}
	return attrSet(h.DataPoints[0].Attributes.ToSlice())
}

func sumAttrs(m metricdata.Metrics) map[string]string {
	s, ok := m.Data.(metricdata.Sum[int64])
	if !ok || len(s.DataPoints) == 0 {
		return nil
	}
	return attrSet(s.DataPoints[0].Attributes.ToSlice())
}

func TestConfigure_RequiresFields(t *testing.T) {
	resetForTest()
	if err := Configure(Config{Environment: "x"}); err == nil {
		t.Fatal("expected error on missing service name")
	}
	if err := Configure(Config{ServiceName: "x"}); err == nil {
		t.Fatal("expected error on missing environment")
	}
}

func TestStatus_Valid(t *testing.T) {
	for _, s := range []Status{StatusSuccess, StatusError, StatusFailure} {
		if !s.Valid() {
			t.Fatalf("expected %q valid", s)
		}
	}
	if Status("nope").Valid() {
		t.Fatal("expected invalid")
	}
}

func TestIncAPI_EmitsAPICounterWithStandardLabels(t *testing.T) {
	r, cleanup := installReader(t)
	defer cleanup()
	mustConfigure(t, nil)

	IncAPI(StatusSuccess, WithRoute("/v1/x"), WithMethod("GET"), WithCode(200))

	m, ok := collectByName(t, r, "api_counter")
	if !ok {
		t.Fatal("api_counter not emitted")
	}
	a := sumAttrs(m)
	if a["service"] != "svc" || a["env"] != "test" || a["status"] != "success" {
		t.Errorf("missing standard labels: %v", a)
	}
	if a["route"] != "/v1/x" || a["method"] != "GET" || a["code"] != "200" {
		t.Errorf("missing option labels: %v", a)
	}
	for _, k := range []string{"pod", "namespace", "host", "ip", "cluster", "region"} {
		if _, present := a[k]; !present {
			t.Errorf("standard label %q missing (value may be empty, but key must be present)", k)
		}
	}
}

func TestRecordAPILatency_EmitsHistogramSeconds(t *testing.T) {
	r, cleanup := installReader(t)
	defer cleanup()
	mustConfigure(t, nil)

	RecordAPILatency(150*time.Millisecond, StatusError, WithRoute("/q"), WithMethod("POST"), WithCode(404))

	m, ok := collectByName(t, r, "api_histogram")
	if !ok {
		t.Fatal("api_histogram not emitted")
	}
	if m.Unit != "s" {
		t.Errorf("expected unit=s, got %q", m.Unit)
	}
	a := histAttrs(m)
	if a["status"] != "error" || a["route"] != "/q" || a["method"] != "POST" || a["code"] != "404" {
		t.Errorf("attrs: %v", a)
	}
}

func TestIncAPI_InvalidStatusFallsBackToFailure(t *testing.T) {
	r, cleanup := installReader(t)
	defer cleanup()
	mustConfigure(t, nil)

	IncAPI(Status("garbage"))

	m, ok := collectByName(t, r, "api_counter")
	if !ok {
		t.Fatal("api_counter missing")
	}
	if sumAttrs(m)["status"] != "failure" {
		t.Errorf("expected fallback failure, got %v", sumAttrs(m))
	}
}

func TestIncCounter_EmitsAdhocWithStandardLabels(t *testing.T) {
	r, cleanup := installReader(t)
	defer cleanup()
	mustConfigure(t, nil)

	if err := IncCounter("widget_total", map[string]string{"color": "red"}); err != nil {
		t.Fatalf("inc: %v", err)
	}
	m, ok := collectByName(t, r, "widget_total")
	if !ok {
		t.Fatal("widget_total missing")
	}
	a := sumAttrs(m)
	if a["color"] != "red" || a["service"] != "svc" || a["env"] != "test" {
		t.Errorf("attrs: %v", a)
	}
}

func TestRecordHistogram_EmitsAdhoc(t *testing.T) {
	r, cleanup := installReader(t)
	defer cleanup()
	mustConfigure(t, nil)

	if err := RecordHistogram("widget_size", 1.5, map[string]string{"kind": "small"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, ok := collectByName(t, r, "widget_size"); !ok {
		t.Fatal("widget_size missing")
	}
}

func TestAdhoc_RejectsAPIPrefix(t *testing.T) {
	mustConfigure(t, nil)
	defer resetForTest()

	if err := IncCounter("api_thing", nil); !errors.Is(err, ErrReservedName) {
		t.Fatalf("expected ErrReservedName, got %v", err)
	}
	if err := RecordHistogram("api_thing", 1, nil); !errors.Is(err, ErrReservedName) {
		t.Fatalf("expected ErrReservedName, got %v", err)
	}
}

func TestAdhoc_NotConfigured(t *testing.T) {
	resetForTest()
	if err := IncCounter("widget", nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
	if err := RecordHistogram("widget", 1, nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
}

func TestAdhoc_EmptyName(t *testing.T) {
	mustConfigure(t, nil)
	defer resetForTest()
	if err := IncCounter("", nil); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestStaticLabels_Override(t *testing.T) {
	_, cleanup := installReader(t)
	defer cleanup()
	mustConfigure(t, map[string]string{"region": "override-region", "extra": "yes"})

	labels := attrSet(StandardLabels())
	if labels["region"] != "override-region" {
		t.Errorf("static label should override region, got %q", labels["region"])
	}
	if labels["extra"] != "yes" {
		t.Errorf("expected extra=yes, got %v", labels)
	}
}

func TestEnvDetection(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "ns-test")
	t.Setenv("NODE_NAME", "node-test")
	t.Setenv("POD_IP", "10.0.0.1")
	t.Setenv("CLUSTER_NAME", "env-cluster")
	t.Setenv("REGION", "env-region")
	resetForTest()
	if err := Configure(Config{ServiceName: "svc", Environment: "test"}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	a := attrSet(StandardLabels())
	if a["namespace"] != "ns-test" || a["host"] != "node-test" || a["ip"] != "10.0.0.1" {
		t.Errorf("env detection: %v", a)
	}
	if a["cluster"] != "env-cluster" || a["region"] != "env-region" {
		t.Errorf("cluster/region from env: %v", a)
	}
}

func TestStandardLabels_ReturnsCopy(t *testing.T) {
	mustConfigure(t, nil)
	defer resetForTest()
	a := StandardLabels()
	if len(a) == 0 {
		t.Fatal("expected non-empty")
	}
	a[0] = attribute.String("hacked", "yes")
	b := StandardLabels()
	for _, kv := range b {
		if string(kv.Key) == "hacked" {
			t.Fatal("StandardLabels returned shared slice")
		}
	}
}
