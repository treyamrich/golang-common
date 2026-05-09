// Package health provides standard /healthz and /readyz HTTP handlers so
// every service in a fleet exposes the same shape to operators, load
// balancers, and Kubernetes probes.
//
// /healthz is a "process is alive" probe — it always returns 200 with a
// JSON body describing the service. /readyz runs caller-supplied checks
// (database reachable, dependency healthy, etc.) and returns 503 with a
// "reasons" array when any check fails.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// defaultCheckTimeout is applied to a Check when its Timeout is zero.
const defaultCheckTimeout = 2 * time.Second

// Check is a single readiness probe.
type Check struct {
	// Name is a stable identifier (e.g. "postgres", "redis", "vault").
	Name string
	// Fn returns nil if healthy, or an error describing why not.
	Fn func(ctx context.Context) error
	// Timeout caps Fn's runtime. Zero defaults to 2s.
	Timeout time.Duration
}

// Response is the JSON body returned by both handlers.
type Response struct {
	Service string   `json:"service"`
	Version string   `json:"version"`
	Commit  string   `json:"commit"`
	Status  string   `json:"status"`
	Reasons []string `json:"reasons,omitempty"`
}

// Handler returns an http.Handler that serves a constant 200 with the
// service identity. Use it as the /healthz endpoint.
func Handler(serviceName, version, commit string) http.Handler {
	body := Response{
		Service: serviceName,
		Version: version,
		Commit:  commit,
		Status:  "ok",
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, body)
	})
}

// ReadinessHandler returns an http.Handler that runs every check on each
// request. If all checks pass the response is 200 + status="ok"; if any
// fail the response is 503 + status="unhealthy" + reasons listing the
// failed checks ("postgres: connection refused", etc.).
func ReadinessHandler(serviceName, version, commit string, checks ...Check) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reasons []string
		for _, c := range checks {
			timeout := c.Timeout
			if timeout <= 0 {
				timeout = defaultCheckTimeout
			}
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			err := c.Fn(ctx)
			cancel()
			if err != nil {
				reasons = append(reasons, c.Name+": "+err.Error())
			}
		}
		resp := Response{
			Service: serviceName,
			Version: version,
			Commit:  commit,
			Status:  "ok",
		}
		status := http.StatusOK
		if len(reasons) > 0 {
			resp.Status = "unhealthy"
			resp.Reasons = reasons
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, resp)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
