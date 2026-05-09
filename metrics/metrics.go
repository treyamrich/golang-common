// Package metrics provides a small, opinionated metrics API on top of the
// OTEL global MeterProvider. It exposes two tiers:
//
//  1. Standard API metrics — preset names (api_counter, api_histogram) and
//     preset label keys. Use these for HTTP/RPC API timing across services
//     so dashboards stay uniform.
//  2. Adhoc metrics — caller picks the name and label keys. Use sparingly.
//     Names starting with "api_" are reserved for the standard tier and
//     are rejected.
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
)

// Valid reports whether s is one of the three defined Status constants.
func (s Status) Valid() bool {
	switch s {
	case StatusSuccess, StatusError, StatusFailure:
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

const (
	standardCounterName   = "api_counter"
	standardHistogramName = "api_histogram"
	meterScope            = "github.com/treyamrich/golang-common/metrics"
)

var (
	cfgMu          sync.RWMutex
	configured     bool
	standardLabels []attribute.KeyValue

	instMu        sync.Mutex
	apiCounter    metric.Int64Counter
	apiHistogram  metric.Float64Histogram
	adhocCounters = map[string]metric.Int64Counter{}
	adhocHistos   = map[string]metric.Float64Histogram{}
)

// ErrNotConfigured is returned by adhoc emitters when Configure has not run.
var ErrNotConfigured = errors.New("metrics: Configure has not been called")

// ErrReservedName is returned when an adhoc metric name uses the "api_"
// prefix reserved for the standard tier.
var ErrReservedName = errors.New("metrics: name prefix \"api_\" is reserved for standard metrics")

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
	apiCounter = nil
	apiHistogram = nil
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

// apiCtx accumulates option-supplied labels for the standard tier.
type apiCtx struct {
	route   string
	method  string
	code    int
	hasCode bool
}

// APIOption configures a standard-tier emit.
type APIOption func(*apiCtx)

// WithRoute attaches a "route" label (server route or client target path).
func WithRoute(route string) APIOption {
	return func(c *apiCtx) { c.route = route }
}

// WithMethod attaches a "method" label (HTTP verb, RPC method).
func WithMethod(method string) APIOption {
	return func(c *apiCtx) { c.method = method }
}

// WithCode attaches a "code" label (HTTP status code, gRPC code).
func WithCode(code int) APIOption {
	return func(c *apiCtx) { c.code = code; c.hasCode = true }
}

func (c *apiCtx) attrs(status Status) []attribute.KeyValue {
	cfgMu.RLock()
	out := make([]attribute.KeyValue, 0, len(standardLabels)+4)
	out = append(out, standardLabels...)
	cfgMu.RUnlock()
	out = append(out, attribute.String("status", string(status)))
	if c.route != "" {
		out = append(out, attribute.String("route", c.route))
	}
	if c.method != "" {
		out = append(out, attribute.String("method", c.method))
	}
	if c.hasCode {
		out = append(out, attribute.String("code", strconv.Itoa(c.code)))
	}
	return out
}

func ensureAPIInstruments() error {
	instMu.Lock()
	defer instMu.Unlock()
	if apiCounter != nil && apiHistogram != nil {
		return nil
	}
	meter := otel.Meter(meterScope)
	c, err := meter.Int64Counter(standardCounterName)
	if err != nil {
		return fmt.Errorf("metrics: api_counter: %w", err)
	}
	h, err := meter.Float64Histogram(standardHistogramName, metric.WithUnit("s"))
	if err != nil {
		return fmt.Errorf("metrics: api_histogram: %w", err)
	}
	apiCounter = c
	apiHistogram = h
	return nil
}

// IncAPI increments the standard api_counter with status + opts. Errors
// during instrument creation are silently swallowed (metrics emission is
// best-effort) but the function returns early so it never panics.
func IncAPI(status Status, opts ...APIOption) {
	if !status.Valid() {
		status = StatusFailure
	}
	if err := ensureAPIInstruments(); err != nil {
		return
	}
	c := &apiCtx{}
	for _, o := range opts {
		o(c)
	}
	apiCounter.Add(context.Background(), 1, metric.WithAttributes(c.attrs(status)...))
}

// RecordAPILatency records d on the standard api_histogram (unit=seconds).
func RecordAPILatency(d time.Duration, status Status, opts ...APIOption) {
	if !status.Valid() {
		status = StatusFailure
	}
	if err := ensureAPIInstruments(); err != nil {
		return
	}
	c := &apiCtx{}
	for _, o := range opts {
		o(c)
	}
	apiHistogram.Record(context.Background(), d.Seconds(), metric.WithAttributes(c.attrs(status)...))
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
	if strings.HasPrefix(name, "api_") {
		return ErrReservedName
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
	apiCounter = nil
	apiHistogram = nil
	adhocCounters = map[string]metric.Int64Counter{}
	adhocHistos = map[string]metric.Float64Histogram{}
	instMu.Unlock()
}
