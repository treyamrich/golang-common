// Package app is the process bootstrap every service and worker uses.
//
// A main is one call:
//
//	func main() {
//	    app.Run("tasks-api", func(ctx context.Context, a *app.App, cfg Config) error {
//	        db, err := postgres.New(cfg.DB)
//	        if err != nil { return err }
//	        if err := a.Start(ctx, "postgres", db); err != nil { return err }
//	        svc := core.NewService(pgstore.New(db))
//	        a.Add(api.New("api", cfg.API, httpin.Handler(svc, verifier)))
//	        return nil
//	    })
//	}
//
// Run owns the lifecycle:
//
//  1. Load the base Config and the service's own config C from env vars.
//     A bad config exits 1 before anything starts.
//  2. Init logging (JSON, cxid-aware) and metrics (Prometheus /metrics).
//  3. Start the admin server (ADMIN_ADDR, default :8081): /healthz,
//     /readyz, /metrics. /readyz stays 503 until setup returns, then
//     checks every started dependency that implements Checker.
//  4. Call setup, which creates dependencies (a.Start) and registers
//     what runs (a.Add, a.Go, a.Every, a.Once).
//  5. Run everything until SIGTERM/SIGINT, a runner fails, or every
//     runner has finished (one-shot jobs).
//  6. Shut down: /readyz goes 503, wait ShutdownDelay so Kubernetes stops
//     routing to us, cancel the runners and wait for them (bounded by
//     ShutdownTimeout), close dependencies in reverse start order, flush
//     metrics, exit 0 (or 1 if anything failed).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/treyamrich/golang-common/config"
	"github.com/treyamrich/golang-common/health"
	"github.com/treyamrich/golang-common/log"
	"github.com/treyamrich/golang-common/otelinit"
)

// Config is the part of the config every process has.
type Config struct {
	Env       string `env:"APP_ENV" envDefault:"dev"`
	Version   string `env:"APP_VERSION" envDefault:"dev"`
	Commit    string `env:"APP_COMMIT"`
	AdminAddr string `env:"ADMIN_ADDR" envDefault:":8081"`
	// ShutdownDelay is how long /readyz reports 503 before runners stop,
	// so endpoints drop the pod before it stops serving.
	ShutdownDelay time.Duration `env:"SHUTDOWN_DELAY" envDefault:"5s"`
	// ShutdownTimeout bounds the whole drain-and-close phase. Keep it
	// under the pod's terminationGracePeriodSeconds.
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"25s"`
}

// Component is a dependency with a lifecycle: a connection pool, a
// client that needs closing. Start should verify the dependency is
// usable (ping), so a misconfigured process fails at startup.
type Component interface {
	Start(ctx context.Context) error
	Close(ctx context.Context) error
}

// Checker is implemented by components that can report readiness.
type Checker interface {
	Check(ctx context.Context) error
}

// Runner is something that runs until its context is cancelled: an HTTP
// server, a queue consumer. Returning nil before cancellation means it
// finished (one-shot work); returning an error stops the process.
type Runner interface {
	Name() string
	Run(ctx context.Context) error
}

// App is handed to setup to register dependencies and runners.
type App struct {
	Name string
	Cfg  Config

	adminAddr string

	mu         sync.Mutex
	components []namedComponent
	runners    []Runner
	ready      atomic.Bool
}

type namedComponent struct {
	name string
	c    Component
}

// Start starts c now and closes it at shutdown, after every runner has
// stopped, in reverse start order. A Start error aborts startup.
func (a *App) Start(ctx context.Context, name string, c Component) error {
	slog.InfoContext(ctx, "starting dependency", "dependency", name)
	if err := c.Start(ctx); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	a.mu.Lock()
	a.components = append(a.components, namedComponent{name, c})
	a.mu.Unlock()
	return nil
}

// AdminAddr is the admin server's listen address.
func (a *App) AdminAddr() string { return a.adminAddr }

// Add registers a Runner.
func (a *App) Add(r Runner) {
	a.mu.Lock()
	a.runners = append(a.runners, r)
	a.mu.Unlock()
}

// Go registers fn as a Runner.
func (a *App) Go(name string, fn func(ctx context.Context) error) { a.Add(runnerFunc{name, fn}) }

type runnerFunc struct {
	name string
	fn   func(context.Context) error
}

func (r runnerFunc) Name() string                  { return r.name }
func (r runnerFunc) Run(ctx context.Context) error { return r.fn(ctx) }

// Run bootstraps the process and never returns. C is the service's
// config struct (see package config); use struct{} if it has none.
func Run[C any](name string, setup func(ctx context.Context, a *App, cfg C) error) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	code := run(ctx, name, setup)
	stop()
	os.Exit(code)
}

// run is Run without the signal handling and os.Exit, for tests. ctx
// being cancelled plays the role of SIGTERM.
func run[C any](sigctx context.Context, name string, setup func(context.Context, *App, C) error) int {
	base, err := config.Load[Config]()
	if err != nil {
		fmt.Fprintln(os.Stderr, "app:", err)
		return 1
	}
	cfg, err := config.Load[C]()
	if err != nil {
		fmt.Fprintln(os.Stderr, "app:", err)
		return 1
	}
	if err := log.Init(log.Config{StaticAttrs: map[string]string{"service": name, "env": base.Env}}); err != nil && !errors.Is(err, log.ErrAlreadyInitialized) {
		fmt.Fprintln(os.Stderr, "app:", err)
		return 1
	}
	shutdownMetrics, err := otelinit.Init(sigctx, otelinit.Config{ServiceName: name, ServiceVersion: base.Version, Environment: base.Env, Insecure: true})
	if err != nil && !errors.Is(err, otelinit.ErrAlreadyInitialized) {
		slog.Error("metrics init failed", "error", err.Error())
		return 1
	}

	a := &App{Name: name, Cfg: base}
	admin, adminLn, err := a.adminServer()
	if err != nil {
		slog.Error("admin server", "error", err.Error())
		return 1
	}
	a.adminAddr = adminLn.Addr().String()
	go func() { _ = admin.Serve(adminLn) }()
	slog.Info("starting", "version", base.Version, "admin_addr", adminLn.Addr().String())

	code := 0
	if err := setup(sigctx, a, cfg); err != nil {
		slog.Error("setup failed", "error", err.Error())
		code = 1
	} else if sigctx.Err() == nil {
		code = a.runAll(sigctx)
	}

	// Shutdown: dependencies after runners, metrics last.
	closeCtx, cancel := context.WithTimeout(context.Background(), base.ShutdownTimeout)
	defer cancel()
	a.closeComponents(closeCtx)
	_ = admin.Shutdown(closeCtx)
	if shutdownMetrics != nil {
		_ = shutdownMetrics(closeCtx)
	}
	slog.Info("stopped", "exit_code", code)
	return code
}

// runAll runs every runner and returns the exit code.
func (a *App) runAll(sigctx context.Context) int {
	a.mu.Lock()
	runners := append([]Runner(nil), a.runners...)
	a.mu.Unlock()
	if len(runners) == 0 {
		slog.Warn("nothing to run")
		return 0
	}
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(sigctx))
	defer cancelRun()

	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(runners))
	// Ready once setup has finished and the runners are launching; a
	// readiness probe's period dwarfs the time a server takes to listen.
	a.ready.Store(true)
	for _, r := range runners {
		go func() {
			defer func() {
				if v := recover(); v != nil {
					results <- result{r.Name(), fmt.Errorf("panic: %v", v)}
				}
			}()
			results <- result{r.Name(), r.Run(runCtx)}
		}()
	}
	slog.Info("ready", "runners", len(runners))

	code, pending := 0, len(runners)
	record := func(res result) {
		pending--
		if res.err != nil && !errors.Is(res.err, context.Canceled) {
			slog.Error("runner failed", "runner", res.name, "error", res.err.Error())
			code = 1
		} else {
			slog.Info("runner finished", "runner", res.name)
		}
	}
	stopping := false
	for pending > 0 && !stopping {
		select {
		case <-sigctx.Done():
			slog.Info("shutdown signal received")
			stopping = true
			a.ready.Store(false)
			if a.Cfg.ShutdownDelay > 0 {
				time.Sleep(a.Cfg.ShutdownDelay)
			}
		case res := <-results:
			record(res)
			if code != 0 {
				stopping = true // one failure stops the process
			}
		}
	}
	a.ready.Store(false)
	cancelRun()
	timeout := time.After(a.Cfg.ShutdownTimeout)
	for pending > 0 {
		select {
		case res := <-results:
			record(res)
		case <-timeout:
			slog.Error("runners did not stop in time", "pending", pending)
			return 1
		}
	}
	return code
}

func (a *App) closeComponents(ctx context.Context) {
	a.mu.Lock()
	cs := append([]namedComponent(nil), a.components...)
	a.mu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		if err := cs[i].c.Close(ctx); err != nil {
			slog.Error("closing dependency", "dependency", cs[i].name, "error", err.Error())
		} else {
			slog.Info("closed dependency", "dependency", cs[i].name)
		}
	}
}

func (a *App) adminServer() (*http.Server, net.Listener, error) {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health.Handler(a.Name, a.Cfg.Version, a.Cfg.Commit))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !a.ready.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable","reasons":["not ready"]}`))
			return
		}
		health.ReadinessHandler(a.Name, a.Cfg.Version, a.Cfg.Commit, a.checks()...).ServeHTTP(w, r)
	})
	mux.Handle("GET /metrics", otelinit.Handler())
	ln, err := net.Listen("tcp", a.Cfg.AdminAddr)
	if err != nil {
		return nil, nil, err
	}
	return &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}, ln, nil
}

func (a *App) checks() []health.Check {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []health.Check
	for _, nc := range a.components {
		if c, ok := nc.c.(Checker); ok {
			out = append(out, health.Check{Name: nc.name, Fn: c.Check})
		}
	}
	return out
}
