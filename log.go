package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// sanitize removes line breaks from a value before it is logged, so it can not
// forge log lines. strings.ReplaceAll is on gosec's list of sanitizers (G706).
func sanitize(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", ""), "\r", "")
}

// errText returns the text of err for a log line. The errors joined by
// errors.Join are separated by "; " instead of line breaks.
func errText(err error) string {
	return strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", ""), "\n", "; ")
}

// parseLogLevel maps the log_level values to a slog level.
func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q", s)
}

// setupLogging makes a text logger with the given (validated) level the
// default, also for the standard log package.
func setupLogging(level string) {
	lvl, _ := parseLogLevel(level)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
}

// fatal logs an error and exits.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
