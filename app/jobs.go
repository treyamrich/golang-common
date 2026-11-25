package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/treyamrich/golang-common/apperr"
	"github.com/treyamrich/golang-common/log"
	"github.com/treyamrich/golang-common/metrics"
)

// Every runs job now and then every interval until shutdown. Each run
// gets its own cxid and records one job_histogram sample. A failed or
// panicking run is logged and the loop carries on; a run in progress at
// shutdown sees its context cancelled.
func (a *App) Every(name string, interval time.Duration, job func(ctx context.Context) error) {
	a.Go(name, func(ctx context.Context) error {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			_ = RunJob(ctx, name, job)
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	})
}

// Once runs job a single time; its error is the process's (exit 1). For
// CronJobs: the process exits when every runner is done.
func (a *App) Once(name string, job func(ctx context.Context) error) {
	a.Go(name, func(ctx context.Context) error { return RunJob(ctx, name, job) })
}

// RunJob runs one job with a fresh cxid, job_histogram metrics and
// panic recovery. Queue consumers call it per message, passing the
// message's cxid on ctx (log.WithCxid) when it carries one.
func RunJob(ctx context.Context, name string, job func(ctx context.Context) error) (err error) {
	if log.FromContext(ctx) == "" {
		ctx = log.WithCxid(ctx, log.NewCxid())
	}
	start := time.Now()
	defer func() {
		if v := recover(); v != nil {
			slog.ErrorContext(ctx, "job panicked", "job", name, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			err = fmt.Errorf("job %s panicked: %v", name, v)
			metrics.RecordJob(name, metrics.StatusFailure, time.Since(start))
		}
	}()
	err = job(ctx)
	status := apperr.Status(err)
	if err != nil && errors.Is(err, context.Canceled) && ctx.Err() == nil {
		status = metrics.StatusFailure
	}
	dur := time.Since(start)
	metrics.RecordJob(name, status, dur)
	attrs := []any{"job", name, "status", string(status), "duration_ms", float64(dur.Microseconds()) / 1000}
	switch status {
	case metrics.StatusFailure:
		slog.ErrorContext(ctx, "job failed", append(attrs, "error", err.Error())...)
	case metrics.StatusError:
		slog.WarnContext(ctx, "job error", append(attrs, "error", err.Error())...)
	default:
		slog.DebugContext(ctx, "job done", attrs...)
	}
	return err
}
