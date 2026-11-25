package log

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestContextHandlerAddsCxid(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil)))
	ctx := WithCxid(context.Background(), "0123456789abcdef0123456789abcdef")

	l.InfoContext(ctx, "with ctx")
	l.Info("no ctx")
	l.With("cxid", "bound").InfoContext(ctx, "bound once")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines=%q", lines)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &m)
	if m["cxid"] != "0123456789abcdef0123456789abcdef" {
		t.Errorf("ctx line: %s", lines[0])
	}
	if strings.Contains(lines[1], "cxid") {
		t.Errorf("no-ctx line: %s", lines[1])
	}
	if strings.Count(lines[2], `"cxid"`) != 1 {
		t.Errorf("bound line has duplicate cxid: %s", lines[2])
	}
}
