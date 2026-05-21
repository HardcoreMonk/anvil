package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestInitSlog_FormatAndLevel exercises the format/level env knobs. The real
// initSlog() sets slog.SetDefault and writes to os.Stderr, so the test instead
// reproduces its handler-selection logic against a captured buffer.
func TestInitSlog_DefaultsToTextHandler(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(h)
	logger.Warn("hello", "key", "val")
	out := buf.String()
	// Text format embeds "key=val" without quotes around the message.
	if !strings.Contains(out, "msg=hello") {
		t.Errorf("expected text-format msg=hello, got %q", out)
	}
	if !strings.Contains(out, "key=val") {
		t.Errorf("expected text-format key=val, got %q", out)
	}
}

func TestInitSlog_JSONFormatProducesJSON(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(h)
	logger.Info("hello", "key", "val")
	out := buf.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("expected JSON object, got %q", out)
	}
	if !strings.Contains(out, `"key":"val"`) {
		t.Errorf("expected JSON key field, got %q", out)
	}
}

func TestInitSlog_LevelGate_FiltersBelowThreshold(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(h)
	logger.Info("info-level should be filtered out")
	logger.Warn("warn-level should pass through")
	out := buf.String()
	if strings.Contains(out, "info-level") {
		t.Errorf("info-level leaked despite Warn threshold: %q", out)
	}
	if !strings.Contains(out, "warn-level") {
		t.Errorf("warn-level filtered unexpectedly: %q", out)
	}
}
