package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/treyamrich/golang-common/internal/metrictest"
)

type fakeDep struct {
	name     string
	events   *[]string
	mu       *sync.Mutex
	checkErr error
}

func (d fakeDep) add(e string)                { d.mu.Lock(); *d.events = append(*d.events, e); d.mu.Unlock() }
func (d fakeDep) Start(context.Context) error { d.add("start " + d.name); return nil }
func (d fakeDep) Close(context.Context) error { d.add("close " + d.name); return nil }
func (d fakeDep) Check(context.Context) error { return d.checkErr }

func testEnv(t *testing.T) {
	t.Setenv("ADMIN_ADDR", "127.0.0.1:0")
	t.Setenv("SHUTDOWN_DELAY", "0s")
	t.Setenv("SHUTDOWN_TIMEOUT", "2s")
}

type none struct{}

func TestLifecycleOrder(t *testing.T) {
	testEnv(t)
	var events []string
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	readyCode := make(chan int, 1)

	code := run(ctx, "svc", func(ctx context.Context, a *App, _ none) error {
		for _, n := range []string{"db", "cache"} {
			if err := a.Start(ctx, n, fakeDep{name: n, events: &events, mu: &mu}); err != nil {
				return err
			}
		}
		a.Go("server", func(ctx context.Context) error {
			resp, err := http.Get("http://" + a.AdminAddr() + "/readyz")
			if err == nil {
				readyCode <- resp.StatusCode
				resp.Body.Close()
			}
			cancel() // SIGTERM
			<-ctx.Done()
			mu.Lock()
			events = append(events, "server stopped")
			mu.Unlock()
			return nil
		})
		return nil
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if c := <-readyCode; c != 200 {
		t.Errorf("readyz while running: %d", c)
	}
	want := []string{"start db", "start cache", "server stopped", "close cache", "close db"}
	if len(events) != len(want) {
		t.Fatalf("events %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events %v want %v", events, want)
		}
	}
}

func TestRunnerFailureExits1AndClosesDeps(t *testing.T) {
	testEnv(t)
	var events []string
	var mu sync.Mutex
	code := run(context.Background(), "svc", func(ctx context.Context, a *App, _ none) error {
		_ = a.Start(ctx, "db", fakeDep{name: "db", events: &events, mu: &mu})
		a.Go("bad", func(context.Context) error { return errors.New("listen: address in use") })
		a.Go("good", func(ctx context.Context) error { <-ctx.Done(); return nil })
		return nil
	})
	if code != 1 || len(events) != 2 || events[1] != "close db" {
		t.Fatalf("code=%d events=%v", code, events)
	}
}

func TestSetupErrorExits1(t *testing.T) {
	testEnv(t)
	if code := run(context.Background(), "svc", func(context.Context, *App, none) error { return errors.New("no db") }); code != 1 {
		t.Fatalf("code=%d", code)
	}
}

func TestConfigErrorExits1(t *testing.T) {
	testEnv(t)
	type needs struct {
		X string `env:"APP_TEST_REQUIRED_X,required"`
	}
	if code := run(context.Background(), "svc", func(context.Context, *App, needs) error { return nil }); code != 1 {
		t.Fatalf("code=%d", code)
	}
}

func TestOnceExitsWhenDone(t *testing.T) {
	testEnv(t)
	r := metrictest.Install(t)
	code := run(context.Background(), "cron", func(_ context.Context, a *App, _ none) error {
		a.Once("sync", func(context.Context) error { return nil })
		return nil
	})
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	_ = r
	code = run(context.Background(), "cron", func(_ context.Context, a *App, _ none) error {
		a.Once("sync", func(context.Context) error { return errors.New("upstream down") })
		return nil
	})
	if code != 1 {
		t.Fatalf("failing job: code=%d", code)
	}
}

func TestRunJob(t *testing.T) {
	r := metrictest.Install(t)
	ctx := context.Background()
	_ = RunJob(ctx, "j", func(context.Context) error { return nil })
	_ = RunJob(ctx, "j", func(context.Context) error { return errors.New("x") })
	if err := RunJob(ctx, "j", func(context.Context) error { panic("boom") }); err == nil {
		t.Fatal("panic not reported")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_ = RunJob(cctx, "j", func(ctx context.Context) error { return ctx.Err() })

	got := r.Counts(t, "job_histogram", "status")
	if got["success"] != 1 || got["failure"] != 2 || got["canceled"] != 1 {
		t.Fatalf("%v", got)
	}
}

func TestEveryLoopsUntilShutdown(t *testing.T) {
	testEnv(t)
	metrictest.Install(t)
	ctx, cancel := context.WithCancel(context.Background())
	runs := 0
	code := run(ctx, "worker", func(_ context.Context, a *App, _ none) error {
		a.Every("tick", 5*time.Millisecond, func(context.Context) error {
			runs++
			if runs == 3 {
				cancel()
			}
			if runs == 2 {
				panic("survivable")
			}
			return nil
		})
		return nil
	})
	if code != 0 || runs < 3 {
		t.Fatalf("code=%d runs=%d", code, runs)
	}
}
