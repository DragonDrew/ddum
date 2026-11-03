package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"": slog.LevelInfo, "debug": slog.LevelDebug, "INFO": slog.LevelInfo,
		"warn": slog.LevelWarn, "warning": slog.LevelWarn, " error ": slog.LevelError,
	} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("trace"); err == nil {
		t.Error("ParseLevel(trace) should fail")
	}
}

func TestNewWritesUTCJSONWithComponent(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelInfo, "bot").Info("hello")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %q", buf.String())
	}
	if rec["component"] != "bot" || rec["msg"] != "hello" {
		t.Fatalf("record = %v", rec)
	}
	ts, _ := rec["time"].(string)
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil || !strings.HasSuffix(ts, "Z") || parsed.IsZero() {
		t.Fatalf("time %q is not RFC 3339 UTC", ts)
	}
}

func TestNewRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelWarn, "agent").Info("hidden")
	if buf.Len() != 0 {
		t.Fatalf("info logged at warn level: %q", buf.String())
	}
}
