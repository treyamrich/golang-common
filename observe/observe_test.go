package observe

import (
	"context"
	"errors"
	"testing"

	"github.com/treyamrich/golang-common/apperr"
	"github.com/treyamrich/golang-common/internal/metrictest"
	"github.com/treyamrich/golang-common/metrics"
)

func TestCallRecordsStatus(t *testing.T) {
	r := metrictest.Install(t)
	ctx := context.Background()

	if v, err := Call(ctx, "postgres", "GetTask", func(context.Context) (int, error) { return 7, nil }); v != 7 || err != nil {
		t.Fatal(v, err)
	}
	_ = Do(ctx, "postgres", "GetTask", func(context.Context) error { return apperr.NotFound("task", 1) })
	_ = Do(ctx, "postgres", "GetTask", func(context.Context) error { return errors.New("conn reset") })

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_ = Do(cctx, "postgres", "GetTask", func(ctx context.Context) error { return ctx.Err() })
	// A dependency saying "canceled" while our ctx is fine is its failure.
	_ = Do(ctx, "postgres", "GetTask", func(context.Context) error { return context.Canceled })

	got := r.Counts(t, "dependency_histogram", "status")
	want := map[string]uint64{"success": 1, "error": 1, "failure": 2, "canceled": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("status %s: got %d want %d (all %v)", k, got[k], v, got)
		}
	}
	if ops := r.Counts(t, "dependency_histogram", "operation"); ops["GetTask"] != 5 {
		t.Errorf("operation label: %v", ops)
	}
}

func TestCallClassifierOverride(t *testing.T) {
	r := metrictest.Install(t)
	_, _ = Call(context.Background(), "spotify", "Search", func(context.Context) (int, error) { return 429, nil },
		func(code int, err error) metrics.Status {
			if code == 429 {
				return metrics.StatusFailure
			}
			return metrics.StatusSuccess
		})
	if got := r.Counts(t, "dependency_histogram", "status"); got["failure"] != 1 {
		t.Fatalf("%v", got)
	}
}

func TestCallPanicIsFailureAndRepanics(t *testing.T) {
	r := metrictest.Install(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		_ = Do(context.Background(), "redis", "Get", func(context.Context) error { panic("boom") })
	}()
	if got := r.Counts(t, "dependency_histogram", "status"); got["failure"] != 1 {
		t.Fatalf("%v", got)
	}
}
