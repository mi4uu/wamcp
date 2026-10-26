package wa

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// slogAdapter routes whatsmeow logs into slog.
type slogAdapter struct {
	l      *slog.Logger
	min    slog.Level
	module string
}

// NewWALogger returns a whatsmeow logger writing to l at or above level
// (debug, info, warn, error).
func NewWALogger(l *slog.Logger, module, level string) waLog.Logger {
	return &slogAdapter{l: l, min: ParseLevel(level), module: module}
}

// ParseLevel converts a level name to a slog level (default info).
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func (a *slogAdapter) log(level slog.Level, msg string, args []any) {
	if level < a.min {
		return
	}
	a.l.Log(context.Background(), level, fmt.Sprintf(msg, args...), "module", a.module)
}

func (a *slogAdapter) Errorf(msg string, args ...any) { a.log(slog.LevelError, msg, args) }
func (a *slogAdapter) Warnf(msg string, args ...any)  { a.log(slog.LevelWarn, msg, args) }
func (a *slogAdapter) Infof(msg string, args ...any)  { a.log(slog.LevelInfo, msg, args) }
func (a *slogAdapter) Debugf(msg string, args ...any) { a.log(slog.LevelDebug, msg, args) }

func (a *slogAdapter) Sub(module string) waLog.Logger {
	return &slogAdapter{l: a.l, min: a.min, module: a.module + "/" + module}
}
