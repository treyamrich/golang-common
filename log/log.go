// Package log provides a small, opinionated wrapper over log/slog that
// every service in the fleet uses so logs share shape, plus correlation-ID
// (cxid) context propagation that ties a single request's logs together
// across services.
//
// Typical use:
//
//	if err := log.Init(log.Config{
//	    Level:       os.Getenv("LOG_LEVEL"),
//	    StaticAttrs: map[string]string{"service": "app-broker"},
//	}); err != nil && !errors.Is(err, log.ErrAlreadyInitialized) {
//	    panic(err)
//	}
//
//	// In a request scope:
//	logger := log.New(ctx)
//	logger.Info("processing request", "user_id", userID)
//
// Output is JSON to stderr with RFC3339Nano timestamps. The cxid (if any)
// stored on ctx is auto-attached as the "cxid" attribute on every line.
package log

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrAlreadyInitialized is returned by Init on subsequent calls. Callers
// (notably in tests) can ignore it.
var ErrAlreadyInitialized = errors.New("log: already initialized")

// Config controls Init.
type Config struct {
	// Level is one of "debug", "info", "warn", "error". Empty = "info".
	// If LOG_LEVEL is set in the environment it overrides this field.
	// Unrecognized values are coerced to "info" with a one-time warning
	// emitted to the configured logger; we do not panic on bad input.
	Level string

	// AddSource includes source file:line on every log record.
	AddSource bool

	// StaticAttrs are attached to every log line. Service identity
	// (service, env, etc.) belongs here.
	StaticAttrs map[string]string

	// Output overrides the default stderr destination. Test-only.
	Output io.Writer
}

type cxidKeyType struct{}

// CxidKey is the context key for the correlation ID. Most callers should
// use FromContext / WithCxid instead of touching the key directly.
var CxidKey = cxidKeyType{}

var (
	initMu     sync.Mutex
	initDone   bool
	staticKVs  []any // pre-rendered slog attrs from Config.StaticAttrs
	staticOnce sync.RWMutex
)

// Init configures the default slog logger. Idempotent: subsequent calls
// return ErrAlreadyInitialized without changing logger state.
func Init(cfg Config) error {
	initMu.Lock()
	defer initMu.Unlock()
	if initDone {
		return ErrAlreadyInitialized
	}

	level, levelWarn := resolveLevel(cfg.Level)

	out := cfg.Output
	if out == nil {
		out = os.Stderr
	}
	handler := slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level:     level,
		AddSource: cfg.AddSource,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					a.Value = slog.StringValue(t.UTC().Format(time.RFC3339Nano))
				}
			}
			return a
		},
	})

	staticOnce.Lock()
	staticKVs = staticKVs[:0]
	for k, v := range cfg.StaticAttrs {
		staticKVs = append(staticKVs, slog.String(k, v))
	}
	staticOnce.Unlock()

	logger := slog.New(handler)
	if len(cfg.StaticAttrs) > 0 {
		logger = logger.With(staticAttrSlice(cfg.StaticAttrs)...)
	}
	slog.SetDefault(logger)

	if levelWarn != "" {
		logger.Warn("log: unrecognized LOG_LEVEL, coerced to info", "input", levelWarn)
	}

	initDone = true
	return nil
}

func staticAttrSlice(m map[string]string) []any {
	out := make([]any, 0, len(m))
	for k, v := range m {
		out = append(out, slog.String(k, v))
	}
	return out
}

// resolveLevel picks the effective slog.Level. Returns the level and, if
// the input was unrecognized, the bad input string for a one-time warning.
// Env LOG_LEVEL overrides cfg.Level.
func resolveLevel(cfgLevel string) (slog.Level, string) {
	raw := os.Getenv("LOG_LEVEL")
	if raw == "" {
		raw = cfgLevel
	}
	if raw == "" {
		return slog.LevelInfo, ""
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, ""
	case "info":
		return slog.LevelInfo, ""
	case "warn", "warning":
		return slog.LevelWarn, ""
	case "error":
		return slog.LevelError, ""
	default:
		return slog.LevelInfo, raw
	}
}

// FromContext returns the cxid stored on ctx, or "" if none is set
// (or ctx is nil).
func FromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(CxidKey).(string)
	return v
}

// WithCxid returns a child context carrying cxid. If ctx is nil, a fresh
// background context is used.
func WithCxid(ctx context.Context, cxid string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, CxidKey, cxid)
}

// New returns a *slog.Logger pre-bound with the cxid (if any) from ctx as
// a structured attribute. Use this in request scope rather than
// slog.Default() so cxid auto-propagates to every line.
//
// Safe to call before Init (uses slog.Default(), which is the standard-
// library default until Init runs). Safe to call with a nil context.
func New(ctx context.Context) *slog.Logger {
	logger := slog.Default()
	cxid := FromContext(ctx)
	if cxid == "" {
		return logger
	}
	return logger.With(slog.String("cxid", cxid))
}

// NewCxid returns a fresh correlation ID — a UUID v4 rendered as 32 hex
// characters with no hyphens. Compact and URL-safe.
func NewCxid() string {
	id := uuid.New()
	hex := id.String() // 8-4-4-4-12 form
	// strip hyphens without allocating a buffer larger than needed
	out := make([]byte, 0, 32)
	for i := 0; i < len(hex); i++ {
		if hex[i] == '-' {
			continue
		}
		out = append(out, hex[i])
	}
	return string(out)
}

// CxidPattern is the regex form of a valid cxid: 32 lowercase hex chars.
const CxidPattern = `^[a-f0-9]{32}$`

// IsValidCxid reports whether s matches CxidPattern. Used by otelhttp to
// reject malformed inbound X-Correlation-Id headers.
func IsValidCxid(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < 32; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// resetForTest clears Init state. Test-only.
func resetForTest() {
	initMu.Lock()
	initDone = false
	initMu.Unlock()
}
