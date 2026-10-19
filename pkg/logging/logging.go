// Package logging configures zerolog as the process logger and routes the
// standard library's slog (used by Echo and adk) into it.
package logging

import (
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/rs/zerolog"
	slogzerolog "github.com/samber/slog-zerolog/v2"
)

// AuditLevel is the level of audit entries: who did what, for whom (a run
// started for a user, a thread token minted, a request refused). It is above
// every built-in level, so no LOG_LEVEL hides it, and is written as "audit".
const AuditLevel zerolog.Level = 10

func init() {
	zerolog.LevelFieldMarshalFunc = func(l zerolog.Level) string {
		if l == AuditLevel {
			return "audit"
		}
		return l.String()
	}
}

// Audit starts an audit entry.
func Audit(l zerolog.Logger) *zerolog.Event { return l.WithLevel(AuditLevel) }

// New builds the root logger and installs it as the slog default.
func New(lvl zerolog.Level, json bool) zerolog.Logger {
	var w io.Writer = os.Stdout
	if !json {
		w = zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.Kitchen}
	}
	logger := zerolog.New(w).Level(lvl).With().Timestamp().Logger()
	slog.SetDefault(slog.New(slogzerolog.Option{Level: slogLevel(lvl), Logger: &logger}.NewZerologHandler()))
	return logger
}

func slogLevel(l zerolog.Level) slog.Level {
	switch {
	case l <= zerolog.DebugLevel:
		return slog.LevelDebug
	case l == zerolog.InfoLevel:
		return slog.LevelInfo
	case l == zerolog.WarnLevel:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}
