// Package slogutil provides configuration and setup utilities for slog.
package slogutil

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Config holds configuration for slog setup.
type Config struct {
	// Level is the minimum log level.
	// Valid values: "debug", "info", "warn", "warning", "error".
	// Default: "info"
	Level string `koanf:"level"`

	// Format is the output format.
	// Valid values: "text", "json".
	// Default: "text"
	Format string `koanf:"format"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Level:  "info",
		Format: "text",
	}
}

// Setup configures the global slog logger based on cfg.
// It sets slog.SetDefault() with the configured handler writing to os.Stderr.
// Returns error if Level or Format contains invalid values.
func Setup(cfg Config) error {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return fmt.Errorf("setup slog: %w", err)
	}

	handler, err := newHandler(cfg.Format, level)
	if err != nil {
		return fmt.Errorf("setup slog: %w", err)
	}

	slog.SetDefault(slog.New(handler))
	return nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrInvalidLevel, s)
	}
}

func newHandler(format string, level slog.Level) (slog.Handler, error) {
	opts := &slog.HandlerOptions{Level: level}

	switch strings.ToLower(format) {
	case "text":
		return slog.NewTextHandler(os.Stderr, opts), nil
	case "json":
		return slog.NewJSONHandler(os.Stderr, opts), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidFormat, format)
	}
}
