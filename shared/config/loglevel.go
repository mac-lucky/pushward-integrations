package config

import (
	"log/slog"
	"os"
	"strings"
)

// LogLevelEnv is the variable every bridge reads to set its log level.
const LogLevelEnv = "PUSHWARD_LOG_LEVEL"

// ParseLogLevel maps a level name to its slog level, case-insensitively. ok is
// false for anything it does not recognise, including the empty string, so the
// caller decides what an unset variable means.
func ParseLogLevel(v string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// NewLogger builds the JSON logger a bridge installs as the slog default, at the
// level named by PUSHWARD_LOG_LEVEL (default info).
//
// Unlike the config env helpers, an unusable value here warns and falls back to
// info rather than refusing to start. Those helpers guard flags that change what
// a user sees, where a misread value is worse than a crash; this one only changes
// what gets written down. Refusing to boot over a diagnostics knob would fail
// hardest at the moment someone is reaching for it, and the fallback is not
// silent - the warning is the first line in the log, emitted through the logger
// this returns, which is why nothing here needs to report an error to a caller
// that has no logger yet.
func NewLogger() *slog.Logger {
	raw := os.Getenv(LogLevelEnv)
	level, ok := ParseLogLevel(raw)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	if raw != "" && !ok {
		logger.Warn("unrecognised log level, defaulting to info",
			"var", LogLevelEnv, "value", raw, "expected", "debug, info, warn or error")
	}
	return logger
}
