package apperr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/treyamrich/golang-common/metrics"
)

func TestKindStatusMapping(t *testing.T) {
	cases := []struct {
		err    error
		code   int
		status metrics.Status
	}{
		{nil, 200, metrics.StatusSuccess},
		{NotFound("task", 7), 404, metrics.StatusError},
		{fmt.Errorf("wrapped: %w", Invalid("bad")), 400, metrics.StatusError},
		{Forbidden("no"), 403, metrics.StatusError},
		{Unauthenticated("no"), 401, metrics.StatusError},
		{Conflict("dup"), 409, metrics.StatusError},
		{Unavailable(errors.New("dial"), "postgres"), 503, metrics.StatusFailure},
		{errors.New("boom"), 500, metrics.StatusFailure},
		{fmt.Errorf("q: %w", context.Canceled), 499, metrics.StatusCanceled},
		{fmt.Errorf("q: %w", context.DeadlineExceeded), 504, metrics.StatusFailure},
		// An explicit kind wins over the context error it wraps.
		{Unavailable(context.Canceled, "postgres"), 503, metrics.StatusFailure},
		{Wrap(context.DeadlineExceeded, KindNotFound, "gone"), 404, metrics.StatusError},
	}
	for _, c := range cases {
		if got := HTTPStatus(c.err); got != c.code {
			t.Errorf("HTTPStatus(%v)=%d want %d", c.err, got, c.code)
		}
		if got := Status(c.err); got != c.status {
			t.Errorf("Status(%v)=%s want %s", c.err, got, c.status)
		}
	}
}

func TestWriteHTTPHidesInternalDetail(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteHTTP(rec, errors.New("password=hunter2"), "abc")
	var b Body
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if rec.Code != 500 || b.Error.Message != "internal error" || b.Error.Code != "internal" || b.Error.CorrelationID != "abc" {
		t.Fatalf("code=%d body=%+v", rec.Code, b)
	}

	rec = httptest.NewRecorder()
	WriteHTTP(rec, Wrap(errors.New("sql detail"), KindNotFound, "task 3 not found"), "")
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if rec.Code != 404 || b.Error.Message != "task 3 not found" {
		t.Fatalf("code=%d body=%+v", rec.Code, b)
	}
}

func TestStatusForHTTP(t *testing.T) {
	for code, want := range map[int]metrics.Status{200: "success", 404: "error", 499: "canceled", 503: "failure"} {
		if got := StatusForHTTP(code); got != want {
			t.Errorf("%d: %s", code, got)
		}
	}
}
