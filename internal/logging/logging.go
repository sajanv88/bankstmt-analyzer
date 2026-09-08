// Package logging builds the process-wide slog logger.
//
// The logger is constructed once in main and injected into everything that
// needs it; nothing here installs a global, so tests can hand components a
// logger writing to a buffer.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// New returns a JSON logger at the given level, writing to w.
//
// Output is JSON in every environment on purpose: development parity with
// production matters more than pretty local output, and the level is the
// only thing that changes.
func New(w io.Writer, level string) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: parseLevel(level),
	})
	return slog.New(handler)
}

// parseLevel maps a LOG_LEVEL string onto a slog level, falling back to
// Info for anything unrecognised rather than failing startup over a typo.
func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
