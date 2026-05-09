package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func decode(t *testing.T, w *httptest.ResponseRecorder) Response {
	t.Helper()
	var r Response
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return r
}

func TestHandler_OK(t *testing.T) {
	h := Handler("svc", "1.2.3", "abc1234")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	r := decode(t, w)
	if r.Service != "svc" || r.Version != "1.2.3" || r.Commit != "abc1234" || r.Status != "ok" {
		t.Errorf("body: %+v", r)
	}
	if ct := w.Header().Get("Content-Type"); ct == "" {
		t.Errorf("missing content-type")
	}
}

func TestReadinessHandler_AllChecksPass(t *testing.T) {
	h := ReadinessHandler("svc", "v", "c",
		Check{Name: "db", Fn: func(context.Context) error { return nil }},
		Check{Name: "cache", Fn: func(context.Context) error { return nil }},
	)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	r := decode(t, w)
	if r.Status != "ok" || len(r.Reasons) != 0 {
		t.Errorf("body: %+v", r)
	}
}

func TestReadinessHandler_FailingCheck(t *testing.T) {
	h := ReadinessHandler("svc", "v", "c",
		Check{Name: "db", Fn: func(context.Context) error { return errors.New("connection refused") }},
		Check{Name: "cache", Fn: func(context.Context) error { return nil }},
	)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	r := decode(t, w)
	if r.Status != "unhealthy" || len(r.Reasons) != 1 {
		t.Fatalf("body: %+v", r)
	}
	if r.Reasons[0] != "db: connection refused" {
		t.Errorf("reason: %s", r.Reasons[0])
	}
}

func TestReadinessHandler_TimeoutFires(t *testing.T) {
	slow := Check{
		Name: "slow",
		Fn: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
				return nil
			}
		},
		Timeout: 10 * time.Millisecond,
	}
	h := ReadinessHandler("svc", "v", "c", slow)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestReadinessHandler_NoChecks(t *testing.T) {
	h := ReadinessHandler("svc", "v", "c")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}
