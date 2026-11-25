// Package metrictest installs an in-memory MeterProvider for tests.
package metrictest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/treyamrich/golang-common/metrics"
)

// Reader collects what the test emitted.
type Reader struct{ r *sdkmetric.ManualReader }

// Install swaps in a fresh MeterProvider and configures metrics.
func Install(t *testing.T) *Reader {
	t.Helper()
	r := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	if err := metrics.Configure(metrics.Config{ServiceName: "test", Environment: "test"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return &Reader{r: r}
}

// Counts returns histogram sample counts for metric name, grouped by the
// value of label.
func (r *Reader) Counts(t *testing.T, name, label string) map[string]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T", name, m.Data)
			}
			for _, dp := range h.DataPoints {
				v, _ := dp.Attributes.Value(attributeKey(label))
				out[v.AsString()] += dp.Count
			}
		}
	}
	return out
}

func attributeKey(k string) attribute.Key { return attribute.Key(k) }
