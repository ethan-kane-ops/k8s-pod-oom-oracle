package cmd

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Log formats.
const (
	logFormatText = "text"
	logFormatJSON = "json"
)

// newLogger builds a structured logger from the level and format flags.
func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	parsed, err := parseLevel(level)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: parsed}

	switch format {
	case logFormatText:
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case logFormatJSON:
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q: want %s or %s", format, logFormatText, logFormatJSON)
	}
}

// parseLevel maps a level name onto a slog level.
func parseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q: want debug, info, warn, or error", level)
	}
}
