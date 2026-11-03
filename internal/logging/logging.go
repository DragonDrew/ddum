// Package logging builds the JSON slog logger used by both binaries.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// ParseLevel accepts debug, info, warn, or error (case-insensitive).
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid log level %q (want debug, info, warn, or error)", s)
}

// New returns a JSON logger at level, tagged with component. Timestamps are
// UTC so logs from hosts in different time zones line up.
func New(w io.Writer, level slog.Level, component string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 && a.Value.Kind() == slog.KindTime {
				a.Value = slog.TimeValue(a.Value.Time().UTC())
			}
			return a
		},
	})
	return slog.New(h).With("component", component)
}
