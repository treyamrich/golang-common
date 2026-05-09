package log

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// captureInit installs a fresh JSON handler writing to a bytes.Buffer for
// assertion. Returns the buffer and a cleanup func.
func captureInit(t *testing.T, cfg Config) *bytes.Buffer {
	t.Helper()
	resetForTest()
	prev := slog.Default()
	t.Cleanup(func() {
		resetForTest()
		slog.SetDefault(prev)
	})
	buf := &bytes.Buffer{}
	cfg.Output = buf
	if err := Init(cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	return buf
}

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestInit_DefaultsToInfo(t *testing.T) {
	buf := captureInit(t, Config{})
	slog.Debug("nope")
	slog.Info("yes")
	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line (info only), got %d", len(lines))
	}
	if lines[0]["level"] != "INFO" || lines[0]["msg"] != "yes" {
		t.Fatalf("unexpected line: %v", lines[0])
	}
	if _, ok := lines[0]["time"].(string); !ok {
		t.Fatalf("missing time: %v", lines[0])
	}
}

func TestInit_LevelFromConfig(t *testing.T) {
	buf := captureInit(t, Config{Level: "debug"})
	slog.Debug("hello")
	lines := decodeLines(t, buf)
	if len(lines) != 1 || lines[0]["level"] != "DEBUG" {
		t.Fatalf("expected debug line, got %v", lines)
	}
}

func TestInit_LogLevelEnvOverridesConfig(t *testing.T) {
	t.Setenv("LOG_LEVEL", "error")
	buf := captureInit(t, Config{Level: "debug"})
	slog.Warn("filtered")
	slog.Error("kept")
	lines := decodeLines(t, buf)
	if len(lines) != 1 || lines[0]["level"] != "ERROR" {
		t.Fatalf("expected only error line, got %v", lines)
	}
}

func TestInit_BadLevelCoercedAndWarns(t *testing.T) {
	buf := captureInit(t, Config{Level: "verbose"})
	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("expected one warn line about bad level, got %d", len(lines))
	}
	if lines[0]["level"] != "WARN" {
		t.Fatalf("expected WARN about bad level, got %v", lines[0])
	}
	if lines[0]["input"] != "verbose" {
		t.Fatalf("expected input=verbose, got %v", lines[0])
	}
}

func TestInit_Idempotent(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)
	if err := Init(Config{Output: &bytes.Buffer{}}); err != nil {
		t.Fatalf("first init: %v", err)
	}
	err := Init(Config{Output: &bytes.Buffer{}})
	if !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("expected ErrAlreadyInitialized, got %v", err)
	}
}

func TestInit_StaticAttrsAttached(t *testing.T) {
	buf := captureInit(t, Config{StaticAttrs: map[string]string{"service": "broker", "env": "test"}})
	slog.Info("hello")
	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if lines[0]["service"] != "broker" || lines[0]["env"] != "test" {
		t.Fatalf("static attrs missing: %v", lines[0])
	}
}

func TestWithCxidFromContext_RoundTrip(t *testing.T) {
	ctx := WithCxid(context.Background(), "abc123")
	if got := FromContext(ctx); got != "abc123" {
		t.Fatalf("expected abc123, got %q", got)
	}
	if got := FromContext(context.Background()); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if got := FromContext(nil); got != "" { //nolint:staticcheck // intentional nil
		t.Fatalf("expected empty for nil ctx, got %q", got)
	}
}

func TestWithCxid_NilContext(t *testing.T) {
	ctx := WithCxid(nil, "xyz") //nolint:staticcheck // intentional nil
	if got := FromContext(ctx); got != "xyz" {
		t.Fatalf("nil-ctx WithCxid lost value")
	}
}

func TestNew_EmitsCxidWhenPresent(t *testing.T) {
	buf := captureInit(t, Config{})
	cxid := NewCxid()
	ctx := WithCxid(context.Background(), cxid)
	New(ctx).Info("hello")
	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if lines[0]["cxid"] != cxid {
		t.Fatalf("expected cxid=%s, got %v", cxid, lines[0]["cxid"])
	}
}

func TestNew_NoCxid_NoAttribute(t *testing.T) {
	buf := captureInit(t, Config{})
	New(context.Background()).Info("hello")
	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if _, ok := lines[0]["cxid"]; ok {
		t.Fatalf("did not expect cxid: %v", lines[0])
	}
}

func TestNew_NilContext_DoesNotPanic(t *testing.T) {
	captureInit(t, Config{})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("New(nil) panicked: %v", r)
		}
	}()
	logger := New(nil) //nolint:staticcheck // intentional nil
	if logger == nil {
		t.Fatal("nil logger")
	}
	logger.Info("ok")
}

func TestNewCxid_Format(t *testing.T) {
	for i := 0; i < 50; i++ {
		c := NewCxid()
		if !IsValidCxid(c) {
			t.Fatalf("NewCxid produced invalid cxid: %q", c)
		}
	}
}

func TestIsValidCxid(t *testing.T) {
	cases := map[string]bool{
		"":                                   false,
		"abc":                                false,
		"00000000000000000000000000000000":   true,
		"deadbeefcafebabe0123456789abcdef00": false, // 34 chars
		"deadbeefcafebabe0123456789abcdeF0":  false, // uppercase F
		"deadbeefcafebabe0123456789abcdef0z": false, // non-hex
	}
	for in, want := range cases {
		if got := IsValidCxid(in); got != want {
			t.Errorf("IsValidCxid(%q) = %v, want %v", in, got, want)
		}
	}
	if !IsValidCxid(NewCxid()) {
		t.Fatal("freshly minted cxid should validate")
	}
}

func TestInit_AddSource(t *testing.T) {
	buf := captureInit(t, Config{AddSource: true})
	slog.Info("trace me")
	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if _, ok := lines[0]["source"]; !ok {
		t.Fatalf("expected source attr, got %v", lines[0])
	}
}
