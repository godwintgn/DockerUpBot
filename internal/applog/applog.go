// Package applog configures process-wide structured logging for container stdout.
package applog

import (
	"log/slog"
	"os"
	"strings"
)

// Init sets the default slog logger to human-readable text on stdout (docker compose logs).
func Init() {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))
}
