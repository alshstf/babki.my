// Package logging настраивает structured-логгер приложения.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// New возвращает slog.Logger с заданным уровнем (debug|info|warn|error)
// и форматом (json|text). Неизвестные значения откатываются к info/json.
func New(level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.ToLower(format) == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(h)
}
