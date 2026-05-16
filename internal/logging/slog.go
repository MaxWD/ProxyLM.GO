package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Init возвращает slog.Logger с JSON-handler'ом и установленным минимальным уровнем.
// Допустимые значения level: "debug", "info", "warn"/"warning", "error".
// При неизвестном значении используется slog.LevelInfo.
//
// Logger дополнительно устанавливается как default через slog.SetDefault,
// чтобы пакеты, использующие slog.Info / slog.Error без явного logger'а,
// писали в тот же поток.
func Init(level string, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	lvl := parseLevel(level)
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info", "":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
