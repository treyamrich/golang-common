// Package api is the inbound HTTP layer every service shares.
//
// New wraps a service's handler (normally its oapi-codegen generated
// server) in the standard middleware chain, outermost first:
//
//  1. CorrelationID: read X-Correlation-Id or mint one; it lands on the
//     context (so every log line and outbound call carries it) and on
//     the response.
//  2. Observe: one api_histogram sample and one access-log line per
//     request. A request whose client went away is recorded as 499 /
//     canceled, never as a 5xx failure.
//  3. Recover: a panic becomes a logged 500 instead of a dead process.
//  4. Timeout and body limit.
//
// Auth is not in this chain: it is declared per operation in the OpenAPI
// spec and enforced by auth.OpenAPI, which the generated server calls.
//
// The api label is "<method>_<route pattern>" from the router's match,
// e.g. "get_/v1/tasks/{id}", for every response including auth
// rejections. Route overrides it. Requests that match no route are
// "unmatched", so the label never takes request data.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/treyamrich/golang-common/apperr"
	"github.com/treyamrich/golang-common/log"
	"github.com/treyamrich/golang-common/metrics"
)

// Config is the HTTP server's config; embed it in the service config.
type Config struct {
	Addr              string        `env:"HTTP_ADDR" envDefault:":8080"`
	ReadHeaderTimeout time.Duration `env:"HTTP_READ_HEADER_TIMEOUT" envDefault:"10s"`
	IdleTimeout       time.Duration `env:"HTTP_IDLE_TIMEOUT" envDefault:"120s"`
	// RequestTimeout bounds each request's context. Zero disables it.
	RequestTimeout time.Duration `env:"HTTP_REQUEST_TIMEOUT" envDefault:"30s"`
	// MaxBodyBytes caps request bodies. Zero disables it.
	MaxBodyBytes int64 `env:"HTTP_MAX_BODY_BYTES" envDefault:"1048576"`
	// ShutdownTimeout is how long in-flight requests get to finish.
	ShutdownTimeout time.Duration `env:"HTTP_SHUTDOWN_TIMEOUT" envDefault:"20s"`
}

// Server is an HTTP server with the standard chain. It satisfies the
// app package's Runner.
type Server struct {
	name string
	cfg  Config
	srv  *http.Server
}

// New returns a Server serving h behind the standard middleware chain.
// name identifies it in logs ("api").
func New(name string, cfg Config, h http.Handler) *Server {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 20 * time.Second
	}
	return &Server{name: name, cfg: cfg, srv: &http.Server{
		Addr:              cfg.Addr,
		Handler:           Chain(h, cfg),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}}
}

// Chain wraps h in the standard middleware, outermost first.
func Chain(h http.Handler, cfg Config) http.Handler {
	h = capturePattern(h)
	h = LimitBody(cfg.MaxBodyBytes)(h)
	h = Timeout(cfg.RequestTimeout)(h)
	h = Recover(h)
	h = Observe(h)
	h = CorrelationID(h)
	return h
}

// Name implements app.Runner.
func (s *Server) Name() string { return s.name }

// Run serves until ctx is cancelled, then drains in-flight requests for
// up to ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("api %s: listen %s: %w", s.name, s.cfg.Addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve is Run on an existing listener (tests use :0).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// Request contexts are not children of ctx: shutdown drains them
	// instead of cancelling them mid-flight.
	s.srv.BaseContext = func(net.Listener) context.Context { return context.WithoutCancel(ctx) }
	errc := make(chan error, 1)
	go func() { errc <- s.srv.Serve(ln) }()
	slog.InfoContext(ctx, "http server listening", "server", s.name, "addr", ln.Addr().String())
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("api %s: shutdown: %w", s.name, err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ---- route label ---------------------------------------------------------

type routeKey struct{}

type routeInfo struct{ api string }

// SetAPI names the current request's api label. StrictOperation and
// Route call it; hand-written handlers rarely need to.
func SetAPI(ctx context.Context, name string) {
	if ri, ok := ctx.Value(routeKey{}).(*routeInfo); ok && ri.api == "" {
		ri.api = name
	}
}

// capturePattern sits directly outside the router: http.ServeMux
// records the matched pattern on the very *Request it was handed, so
// after it returns, r.Pattern names the route ("GET /v1/tasks/{id}").
func capturePattern(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.Pattern != "" {
			SetAPI(r.Context(), PatternLabel(r.Method, r.Pattern))
		}
	})
}

// PatternLabel turns a ServeMux pattern into the api label:
// "GET /v1/tasks/{id}" -> "get_/v1/tasks/{id}". A pattern without a
// method takes the request's.
func PatternLabel(method, pattern string) string {
	if m, path, ok := strings.Cut(pattern, " "); ok {
		method, pattern = m, strings.TrimSpace(path)
	}
	return strings.ToLower(method) + "_" + pattern
}

// Route overrides the label for one handler: mux.Handle("GET /x", api.Route("getX", h)).
func Route(name string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetAPI(r.Context(), name)
		h.ServeHTTP(w, r)
	})
}

// RequestError is the strict server's RequestErrorHandlerFunc: a request
// that doesn't parse against the spec is the caller's 400.
func RequestError(w http.ResponseWriter, r *http.Request, err error) {
	apperr.WriteHTTP(w, apperr.Wrap(err, apperr.KindInvalid, "%s", err.Error()), log.FromContext(r.Context()))
}

// ResponseError is the strict server's ResponseErrorHandlerFunc: an error
// a handler returned instead of a typed response. apperr kinds map to
// their status; anything else is a 500 whose detail stays in the log.
func ResponseError(w http.ResponseWriter, r *http.Request, err error) {
	if apperr.Status(err) == metrics.StatusFailure {
		slog.ErrorContext(r.Context(), "request failed", "error", err.Error())
	}
	apperr.WriteHTTP(w, err, log.FromContext(r.Context()))
}

// ---- middleware ----------------------------------------------------------

// CorrelationID reads a valid inbound X-Correlation-Id or mints one,
// stores it on the context and echoes it on the response.
func CorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cxid := r.Header.Get(log.CorrelationIDHeader)
		if !log.IsValidCxid(cxid) {
			cxid = log.NewCxid()
		}
		w.Header().Set(log.CorrelationIDHeader, cxid)
		next.ServeHTTP(w, r.WithContext(log.WithCxid(r.Context(), cxid)))
	})
}

// StatusClientClosedRequest is recorded when the client disconnects
// before the response (nginx's convention).
const StatusClientClosedRequest = 499

// Observe records api_histogram and writes one access-log line per
// request.
func Observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ri := &routeInfo{}
		sw := &statusWriter{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), routeKey{}, ri))
		next.ServeHTTP(sw, r)

		code := sw.code()
		if errors.Is(r.Context().Err(), context.Canceled) {
			// The client left. Whatever the handler made of its cancelled
			// context (usually a 500) is not our failure.
			code = StatusClientClosedRequest
		}
		name := ri.api
		if name == "" {
			name = "unmatched"
		}
		status := apperr.StatusForHTTP(code)
		dur := time.Since(start)
		metrics.RecordAPI(name, code, status, dur)

		level := slog.LevelInfo
		if status == metrics.StatusFailure {
			level = slog.LevelError
		}
		slog.Log(r.Context(), level, "request",
			"api", name, "method", r.Method, "path", r.URL.Path,
			"http_status", code, "status", string(status),
			"duration_ms", float64(dur.Microseconds())/1000, "bytes", sw.bytes)
	})
}

// Recover turns a panic into a logged 500.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			slog.ErrorContext(r.Context(), "panic", "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			if sw, ok := w.(*statusWriter); !ok || sw.status == 0 {
				apperr.WriteHTTP(w, errors.New("panic"), log.FromContext(r.Context()))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Timeout bounds each request's context; zero disables it.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// LimitBody caps the request body; zero disables it.
func LimitBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if n <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusWriter) code() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

// Unwrap lets http.ResponseController reach Flush and friends.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
