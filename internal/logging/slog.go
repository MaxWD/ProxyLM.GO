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
	levelVar.Set(parseLevel(level))
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: &levelVar})
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger
}

// levelVar — динамический уровень логирования, общий для всех logger'ов Init;
// меняется на лету через SetLevel (hot-reload proxy.log_level).
var levelVar slog.LevelVar

// SetLevel меняет минимальный уровень логирования у уже созданных logger'ов.
// Принимает те же значения, что Init: debug, info, warn/warning, error.
func SetLevel(level string) { levelVar.Set(parseLevel(level)) }

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
