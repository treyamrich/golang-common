package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/treyamrich/golang-common/apperr"
	"github.com/treyamrich/golang-common/internal/metrictest"
	"github.com/treyamrich/golang-common/log"
)

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	Chain(h, Config{RequestTimeout: time.Second, MaxBodyBytes: 8}).ServeHTTP(rec, req)
	return rec
}

func TestCorrelationID(t *testing.T) {
	var seen string
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = log.FromContext(r.Context()) })

	rec := serve(h, httptest.NewRequest("GET", "/", nil))
	if !log.IsValidCxid(seen) || rec.Header().Get(log.CorrelationIDHeader) != seen {
		t.Fatalf("minted cxid %q, header %q", seen, rec.Header().Get(log.CorrelationIDHeader))
	}
	in := log.NewCxid()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(log.CorrelationIDHeader, in)
	serve(h, req)
	if seen != in {
		t.Fatalf("inbound cxid not kept: %q", seen)
	}
	req.Header.Set(log.CorrelationIDHeader, "not-valid")
	serve(h, req)
	if seen == "not-valid" {
		t.Fatal("malformed cxid accepted")
	}
}

func TestObserveLabelsAndStatuses(t *testing.T) {
	r := metrictest.Install(t)
	mux := http.NewServeMux()
	mux.Handle("GET /tasks/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "missing" {
			apperr.WriteHTTP(w, apperr.NotFound("task", "missing"), "")
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	mux.Handle("GET /boom", Route("boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("x") })))

	serve(mux, httptest.NewRequest("GET", "/tasks/1", nil))
	serve(mux, httptest.NewRequest("GET", "/tasks/2", nil))
	serve(mux, httptest.NewRequest("GET", "/tasks/missing", nil))
	serve(mux, httptest.NewRequest("GET", "/nope", nil))
	if rec := serve(mux, httptest.NewRequest("GET", "/boom", nil)); rec.Code != 500 {
		t.Fatalf("panic code %d", rec.Code)
	}

	apis := r.Counts(t, "api_histogram", "api")
	if apis["get_/tasks/{id}"] != 3 || apis["unmatched"] != 1 || apis["boom"] != 1 || len(apis) != 3 {
		t.Errorf("api label: %v", apis)
	}
	codes := r.Counts(t, "api_histogram", "http_status")
	if codes["200"] != 2 || codes["404"] != 2 || codes["500"] != 1 {
		t.Errorf("http_status: %v", codes)
	}
	st := r.Counts(t, "api_histogram", "status")
	if st["success"] != 2 || st["error"] != 2 || st["failure"] != 1 {
		t.Errorf("status: %v", st)
	}
}

// A client that disconnects mid-request is 499/canceled even though the
// handler, seeing its context cancelled, answers 500.
func TestClientDisconnectIs499(t *testing.T) {
	r := metrictest.Install(t)
	started := make(chan struct{})
	done := make(chan struct{})
	h := Route("slow", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		ResponseError(w, r, r.Context().Err())
		close(done)
	}))
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = New("t", Config{}, h).Serve(ctx, ln) }()

	cctx, ccancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(cctx, "GET", "http://"+ln.Addr().String()+"/", nil)
	go func() { <-started; ccancel() }()
	_, _ = http.DefaultClient.Do(req)
	<-done
	// Observe records right after the handler returns.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.Counts(t, "api_histogram", "http_status")["499"] == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := r.Counts(t, "api_histogram", "status"); got["canceled"] != 1 || got["failure"] != 0 {
		t.Fatalf("status: %v", got)
	}
}

func TestPatternLabel(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"GET", "GET /v1/tasks/{id}"}: "get_/v1/tasks/{id}",
		{"POST", "/legacy"}:           "post_/legacy",
	} {
		if got := PatternLabel(in[0], in[1]); got != want {
			t.Errorf("%v: %s", in, got)
		}
	}
}

func TestTimeoutIs504(t *testing.T) {
	metrictest.Install(t)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		ResponseError(w, r, r.Context().Err())
	})
	rec := httptest.NewRecorder()
	Chain(h, Config{RequestTimeout: 10 * time.Millisecond}).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 504 {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestLimitBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				w.WriteHeader(413)
			}
		}
	})
	if rec := serve(h, httptest.NewRequest("POST", "/", strings.NewReader("way more than 8 bytes"))); rec.Code != 413 {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestServerShutsDownOnCancel(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- New("t", Config{}, http.NotFoundHandler()).Serve(ctx, ln) }()
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
}
