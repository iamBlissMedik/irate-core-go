// Package logging builds the structured application logger (log/slog).
package logging

import (
	"io"
	"log/slog"
)

// New returns a JSON logger (production; what log aggregators expect) or a
// human-readable text logger (development).
func New(w io.Writer, level slog.Level, format, env string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h).With("service", "irate-core-go", "env", env)
}
