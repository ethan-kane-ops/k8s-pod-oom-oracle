package cmd

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		level   string
		want    slog.Level
		wantErr bool
	}{
		{name: "debug", level: "debug", want: slog.LevelDebug},
		{name: "info", level: "info", want: slog.LevelInfo},
		{name: "warn", level: "warn", want: slog.LevelWarn},
		{name: "warning is accepted as warn", level: "warning", want: slog.LevelWarn},
		{name: "error", level: "error", want: slog.LevelError},
		{name: "case is ignored", level: "DEBUG", want: slog.LevelDebug},
		{name: "mixed case is ignored", level: "Warn", want: slog.LevelWarn},
		{name: "unknown", level: "verbose", wantErr: true},
		{name: "empty", level: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseLevel(tt.level)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseLevel(%q) = %v, want an error", tt.level, got)
				}
				// The message has to name the accepted values, since a typo in
				// a DaemonSet arg is otherwise a crash loop with no clue in it.
				if !strings.Contains(err.Error(), "debug") {
					t.Errorf("error %q does not list the valid levels", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLevel(%q) error = %v", tt.level, err)
			}
			if got != tt.want {
				t.Errorf("parseLevel(%q) = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

func TestNewLoggerFormats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		format   string
		wantJSON bool
	}{
		{name: "text", format: "text"},
		{name: "json", format: "json", wantJSON: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			log, err := newLogger(&buf, "info", tt.format)
			if err != nil {
				t.Fatalf("newLogger() error = %v", err)
			}

			log.Info("hello", "key", "value")

			line := strings.TrimSpace(buf.String())
			if line == "" {
				t.Fatal("logger wrote nothing")
			}

			var decoded map[string]any
			isJSON := json.Unmarshal([]byte(line), &decoded) == nil
			if isJSON != tt.wantJSON {
				t.Errorf("output is JSON = %t, want %t (line %q)", isJSON, tt.wantJSON, line)
			}
			if !strings.Contains(line, "hello") || !strings.Contains(line, "value") {
				t.Errorf("output %q is missing the message or its attributes", line)
			}
		})
	}
}

func TestNewLoggerRejectsBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		level  string
		format string
	}{
		{name: "unknown format", level: "info", format: "logfmt"},
		{name: "unknown level", level: "trace", format: "text"},
		{name: "empty format", level: "info", format: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := newLogger(&bytes.Buffer{}, tt.level, tt.format); err == nil {
				t.Fatalf("newLogger(%q, %q) = nil error, want an error", tt.level, tt.format)
			}
		})
	}
}

// The level is a filter, not a label. A daemon started at info must not pay to
// format debug records it will discard.
func TestNewLoggerHonoursTheLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log, err := newLogger(&buf, "warn", "text")
	if err != nil {
		t.Fatalf("newLogger() error = %v", err)
	}

	log.Debug("debug message")
	log.Info("info message")
	if buf.Len() != 0 {
		t.Errorf("records below the configured level were written: %q", buf.String())
	}

	log.Warn("warn message")
	if !strings.Contains(buf.String(), "warn message") {
		t.Errorf("output %q is missing the record at the configured level", buf.String())
	}
}
