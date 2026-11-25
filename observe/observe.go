// Package observe instruments outbound dependency calls.
//
// Every call a service makes to something it doesn't own (its database,
// another service, a third-party API) goes through Call or Do, which
// records one dependency_histogram sample:
//
//	dependency_histogram{service, pod, env, ..., dependency, operation, status}
//
// The histogram's _count is the call counter. dependency and operation
// must be constants: put them in the adapter, e.g.
//
//	tasks, err := observe.Call(ctx, "postgres", "ListTasks", func(ctx context.Context) ([]core.Task, error) {
//	    return s.q.ListTasks(ctx)
//	})
//
// Status comes from the classifier: by default apperr.Status, so nil is
// success, our own cancelled context is canceled, client-caused apperr
// kinds are error and anything else is failure. A panic inside fn is
// recorded as failure and re-panicked; recovering it is the caller's job
// (the api package's Recover middleware, app's job loops).
package observe

import (
	"context"
	"errors"
	"time"

	"github.com/treyamrich/golang-common/apperr"
	"github.com/treyamrich/golang-common/metrics"
)

// Classifier decides a call's metrics status from its result.
type Classifier[T any] func(res T, err error) metrics.Status

// Call runs fn, records dependency_histogram and returns fn's result.
// Pass at most one classifier to override the default.
func Call[T any](ctx context.Context, dependency, operation string, fn func(context.Context) (T, error), classify ...Classifier[T]) (res T, err error) {
	start := time.Now()
	finished := false
	defer func() {
		if finished {
			return
		}
		// fn panicked: record it, then let the panic continue.
		metrics.RecordDependency(dependency, operation, metrics.StatusFailure, time.Since(start))
	}()
	res, err = fn(ctx)
	finished = true
	status := DefaultStatus(ctx, err)
	if len(classify) > 0 && classify[0] != nil {
		status = classify[0](res, err)
	}
	metrics.RecordDependency(dependency, operation, status, time.Since(start))
	return res, err
}

// Do is Call for functions that return only an error.
func Do(ctx context.Context, dependency, operation string, fn func(context.Context) error, classify ...Classifier[struct{}]) error {
	_, err := Call(ctx, dependency, operation, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	}, classify...)
	return err
}

// DefaultStatus is the default classifier. A context.Canceled error only
// counts as canceled when ctx itself was cancelled (the caller left or
// we're shutting down); a dependency returning "canceled" on its own is
// a failure.
func DefaultStatus(ctx context.Context, err error) metrics.Status {
	if errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return metrics.StatusFailure
	}
	return apperr.Status(err)
}
