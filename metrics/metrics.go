// Package metrics provides a small, opinionated metrics API on top of the
// OTEL global MeterProvider. It exposes two tiers:
//
//  1. Standard metrics — three histograms with preset label keys, so
//     dashboards stay uniform across services:
//     api_histogram{api, http_status, status}            inbound requests
//     dependency_histogram{dependency, operation, status} outbound calls
//     job_histogram{job, status}                          worker runs
//     Each histogram's _count is the call counter. Services normally
//     don't call these directly: the api package, observe.Call and
//     app's job loops do.
//  2. Adhoc metrics — caller picks the name and label keys. Use sparingly.
//     The api_, dependency_ and job_ prefixes are reserved.
//
// Every metric (standard and adhoc) auto-attaches a "standard label set"
// describing the runtime context (service, env, pod, namespace, host, ip,
// cluster, region). The set is detected once at Init time from
// otelinit.Config plus environment variables (the Kubernetes downward API
// names — POD_NAMESPACE, NODE_NAME, POD_IP — and a few cloud equivalents).
//
// Note: "version" is intentionally NOT a standard label. Including a
// version label resets the time series on every deploy and breaks
// dashboard continuity. Service version belongs in /healthz and on
// trace resource attributes, not on metric series.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Status is the standard outcome label for the api_* standard metrics.
type Status string

const (
	// StatusSuccess — happy-path completion.
	StatusSuccess Status = "success"
	// StatusError — expected failure (validation, 4xx, consent revoked).
	StatusError Status = "error"
	// StatusFailure — unexpected failure (5xx, panic, dependency down).
	StatusFailure Status = "failure"
	// StatusCanceled — the caller went away (client disconnect, our own
	// context cancelled during shutdown). Not an error on our side, so
	// it is kept out of error and failure rates.
	StatusCanceled Status = "canceled"
)

// Valid reports whether s is one of the defined Status constants.
func (s Status) Valid() bool {
	switch s {
	case StatusSuccess, StatusError, StatusFailure, StatusCanceled:
		return true
	}
	return false
}

// Config is the input to Configure. It is normally populated from the
// otelinit.Config so callers don't pass values twice.
type Config struct {
	ServiceName  string            // required
	Environment  string            // required
	Cluster      string            // optional
	Region       string            // optional
	StaticLabels map[string]string // optional — attached to every metric, override auto-detected values
}

const meterScope = "github.com/treyamrich/golang-common/metrics"

var (
	cfgMu          sync.RWMutex
	configured     bool
	standardLabels []attribute.KeyValue

	instMu        sync.Mutex
	apiHistogram  metric.Float64Histogram
	depHistogram  metric.Float64Histogram
	jobHistogram  metric.Float64Histogram
	adhocCounters = map[string]metric.Int64Counter{}
	adhocHistos   = map[string]metric.Float64Histogram{}
)

// ErrNotConfigured is returned by adhoc emitters when Configure has not run.
var ErrNotConfigured = errors.New("metrics: Configure has not been called")

// ErrReservedName is returned when an adhoc metric name uses a prefix
// reserved for the standard tier ("api_", "dependency_", "job_").
var ErrReservedName = errors.New("metrics: name prefixes api_, dependency_ and job_ are reserved for standard metrics")

// Configure sets the standard label set from cfg + auto-detected
// environment values. It is idempotent — subsequent calls overwrite the
// label set and reset cached instruments. Most callers do not invoke
// Configure directly — otelinit.Init calls it.
func Configure(cfg Config) error {
	if cfg.ServiceName == "" {
		return errors.New("metrics: ServiceName is required")
	}
	if cfg.Environment == "" {
		return errors.New("metrics: Environment is required")
	}

	labels := buildStandardLabels(cfg)

	cfgMu.Lock()
	standardLabels = labels
	configured = true
	cfgMu.Unlock()

	// Reset cached instruments so the next emit binds to whatever
	// MeterProvider is currently installed on the OTEL global.
	instMu.Lock()
	apiHistogram = nil
	depHistogram = nil
	jobHistogram = nil
	adhocCounters = map[string]metric.Int64Counter{}
	adhocHistos = map[string]metric.Float64Histogram{}
	instMu.Unlock()
	return nil
}

// IsConfigured reports whether Configure has been called.
func IsConfigured() bool {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return configured
}

// StandardLabels returns a copy of the auto-attached standard label set.
// Useful in tests to assert exact label emission.
func StandardLabels() []attribute.KeyValue {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	out := make([]attribute.KeyValue, len(standardLabels))
	copy(out, standardLabels)
	return out
}

func buildStandardLabels(cfg Config) []attribute.KeyValue {
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := os.Getenv(k); v != "" {
				return v
			}
		}
		return ""
	}

	pod := get("HOSTNAME")
	if pod == "" {
		pod, _ = os.Hostname()
	}

	host := get("NODE_NAME", "EC2_INSTANCE_ID")
	if host == "" {
		host, _ = os.Hostname()
	}

	ip := get("POD_IP")
	if ip == "" {
		ip = firstNonLoopbackIP()
	}

	cluster := cfg.Cluster
	if cluster == "" {
		cluster = os.Getenv("CLUSTER_NAME")
	}
	region := cfg.Region
	if region == "" {
		region = os.Getenv("REGION")
	}

	base := map[string]string{
		"service":   cfg.ServiceName,
		"env":       cfg.Environment,
		"pod":       pod,
		"namespace": os.Getenv("POD_NAMESPACE"),
		"host":      host,
		"ip":        ip,
		"cluster":   cluster,
		"region":    region,
	}
	for k, v := range cfg.StaticLabels {
		base[k] = v
	}

	out := make([]attribute.KeyValue, 0, len(base))
	for k, v := range base {
		out = append(out, attribute.String(k, v))
	}
	return out
}

func firstNonLoopbackIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

// The three standard histograms. Each one's _count series is the call
// counter, so no separate counters are emitted.
const (
	apiHistogramName        = "api_histogram"
	dependencyHistogramName = "dependency_histogram"
	jobHistogramName        = "job_histogram"
)

func ensureStandard() error {
	instMu.Lock()
	defer instMu.Unlock()
	if apiHistogram != nil {
		return nil
	}
	meter := otel.Meter(meterScope)
	var err error
	if apiHistogram, err = meter.Float64Histogram(apiHistogramName, metric.WithUnit("s"),
		metric.WithDescription("Inbound API requests: latency in seconds, labelled by api, http_status and status.")); err != nil {
		return fmt.Errorf("metrics: %s: %w", apiHistogramName, err)
	}
	if depHistogram, err = meter.Float64Histogram(dependencyHistogramName, metric.WithUnit("s"),
		metric.WithDescription("Outbound dependency calls: latency in seconds, labelled by dependency, operation and status.")); err != nil {
		return fmt.Errorf("metrics: %s: %w", dependencyHistogramName, err)
	}
	if jobHistogram, err = meter.Float64Histogram(jobHistogramName, metric.WithUnit("s"),
		metric.WithDescription("Worker job runs: latency in seconds, labelled by job and status.")); err != nil {
		return fmt.Errorf("metrics: %s: %w", jobHistogramName, err)
	}
	return nil
}

func withStandard(extra ...attribute.KeyValue) metric.MeasurementOption {
	cfgMu.RLock()
	out := make([]attribute.KeyValue, 0, len(standardLabels)+len(extra))
	out = append(out, standardLabels...)
	cfgMu.RUnlock()
	out = append(out, extra...)
	return metric.WithAttributes(out...)
}

func validStatus(s Status) Status {
	if !s.Valid() {
		return StatusFailure
	}
	return s
}

// RecordAPI records one inbound request on api_histogram.
//
// api is the route's stable name (the OpenAPI operationId, or
// "<method>_<pattern>" for hand-registered routes), never a raw URL
// path: label values must be bounded.
func RecordAPI(api string, httpStatus int, status Status, d time.Duration) {
	if ensureStandard() != nil {
		return
	}
	apiHistogram.Record(context.Background(), d.Seconds(), withStandard(
		attribute.String("api", api),
		attribute.String("http_status", strconv.Itoa(httpStatus)),
		attribute.String("status", string(validStatus(status))),
	))
}

// RecordDependency records one outbound call on dependency_histogram.
// dependency ("postgres", "spotify") and operation ("ListTasks",
// "GET") must be constants, never request data.
func RecordDependency(dependency, operation string, status Status, d time.Duration) {
	if ensureStandard() != nil {
		return
	}
	depHistogram.Record(context.Background(), d.Seconds(), withStandard(
		attribute.String("dependency", dependency),
		attribute.String("operation", operation),
		attribute.String("status", string(validStatus(status))),
	))
}

// RecordJob records one worker job run on job_histogram.
func RecordJob(job string, status Status, d time.Duration) {
	if ensureStandard() != nil {
		return
	}
	jobHistogram.Record(context.Background(), d.Seconds(), withStandard(
		attribute.String("job", job),
		attribute.String("status", string(validStatus(status))),
	))
}

// IncCounter increments an adhoc counter named name. Returns an error if
// name is empty, uses the reserved "api_" prefix, or Configure has not run.
func IncCounter(name string, labels map[string]string) error {
	if err := validateAdhocName(name); err != nil {
		return err
	}
	if !IsConfigured() {
		return ErrNotConfigured
	}
	c, err := getAdhocCounter(name)
	if err != nil {
		return err
	}
	c.Add(context.Background(), 1, metric.WithAttributes(adhocAttrs(labels)...))
	return nil
}

// RecordHistogram records value on an adhoc histogram named name.
func RecordHistogram(name string, value float64, labels map[string]string) error {
	if err := validateAdhocName(name); err != nil {
		return err
	}
	if !IsConfigured() {
		return ErrNotConfigured
	}
	h, err := getAdhocHistogram(name)
	if err != nil {
		return err
	}
	h.Record(context.Background(), value, metric.WithAttributes(adhocAttrs(labels)...))
	return nil
}

func validateAdhocName(name string) error {
	if name == "" {
		return errors.New("metrics: name is required")
	}
	for _, p := range []string{"api_", "dependency_", "job_"} {
		if strings.HasPrefix(name, p) {
			return ErrReservedName
		}
	}
	return nil
}

func adhocAttrs(labels map[string]string) []attribute.KeyValue {
	cfgMu.RLock()
	out := make([]attribute.KeyValue, 0, len(standardLabels)+len(labels))
	out = append(out, standardLabels...)
	cfgMu.RUnlock()
	for k, v := range labels {
		out = append(out, attribute.String(k, v))
	}
	return out
}

func getAdhocCounter(name string) (metric.Int64Counter, error) {
	instMu.Lock()
	defer instMu.Unlock()
	if c, ok := adhocCounters[name]; ok {
		return c, nil
	}
	c, err := otel.Meter(meterScope).Int64Counter(name)
	if err != nil {
		return nil, fmt.Errorf("metrics: adhoc counter %q: %w", name, err)
	}
	adhocCounters[name] = c
	return c, nil
}

func getAdhocHistogram(name string) (metric.Float64Histogram, error) {
	instMu.Lock()
	defer instMu.Unlock()
	if h, ok := adhocHistos[name]; ok {
		return h, nil
	}
	h, err := otel.Meter(meterScope).Float64Histogram(name)
	if err != nil {
		return nil, fmt.Errorf("metrics: adhoc histogram %q: %w", name, err)
	}
	adhocHistos[name] = h
	return h, nil
}

// resetForTest clears all configured state. Test-only.
func resetForTest() {
	cfgMu.Lock()
	configured = false
	standardLabels = nil
	cfgMu.Unlock()
	instMu.Lock()
	apiHistogram = nil
	depHistogram = nil
	jobHistogram = nil
	adhocCounters = map[string]metric.Int64Counter{}
	adhocHistos = map[string]metric.Float64Histogram{}
	instMu.Unlock()
}
