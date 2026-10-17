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
