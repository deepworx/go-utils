package slogutil

import (
	"errors"
	"log/slog"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	if cfg.Level != "info" {
		t.Errorf("Level = %q, want %q", cfg.Level, "info")
	}
	if cfg.Format != "text" {
		t.Errorf("Format = %q, want %q", cfg.Format, "text")
	}
}

func TestSetup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{
			name:    "default config",
			cfg:     DefaultConfig(),
			wantErr: nil,
		},
		{
			name:    "debug level json format",
			cfg:     Config{Level: "debug", Format: "json"},
			wantErr: nil,
		},
		{
			name:    "warn level",
			cfg:     Config{Level: "warn", Format: "text"},
			wantErr: nil,
		},
		{
			name:    "warning alias",
			cfg:     Config{Level: "warning", Format: "text"},
			wantErr: nil,
		},
		{
			name:    "error level",
			cfg:     Config{Level: "error", Format: "json"},
			wantErr: nil,
		},
		{
			name:    "case insensitive level",
			cfg:     Config{Level: "DEBUG", Format: "TEXT"},
			wantErr: nil,
		},
		{
			name:    "invalid level",
			cfg:     Config{Level: "trace", Format: "text"},
			wantErr: ErrInvalidLevel,
		},
		{
			name:    "invalid format",
			cfg:     Config{Level: "info", Format: "xml"},
			wantErr: ErrInvalidFormat,
		},
		{
			name:    "empty level",
			cfg:     Config{Level: "", Format: "text"},
			wantErr: ErrInvalidLevel,
		},
		{
			name:    "empty format",
			cfg:     Config{Level: "info", Format: ""},
			wantErr: ErrInvalidFormat,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := Setup(tt.cfg)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("Setup() error = %v, want %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Errorf("Setup() unexpected error: %v", err)
			}
		})
	}
}

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input   string
		want    slog.Level
		wantErr error
	}{
		{"debug", slog.LevelDebug, nil},
		{"DEBUG", slog.LevelDebug, nil},
		{"Debug", slog.LevelDebug, nil},
		{"info", slog.LevelInfo, nil},
		{"INFO", slog.LevelInfo, nil},
		{"warn", slog.LevelWarn, nil},
		{"WARN", slog.LevelWarn, nil},
		{"warning", slog.LevelWarn, nil},
		{"WARNING", slog.LevelWarn, nil},
		{"error", slog.LevelError, nil},
		{"ERROR", slog.LevelError, nil},
		{"trace", 0, ErrInvalidLevel},
		{"", 0, ErrInvalidLevel},
		{"invalid", 0, ErrInvalidLevel},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()

			got, err := parseLevel(tt.input)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("parseLevel(%q) error = %v, want %v", tt.input, err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Errorf("parseLevel(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("parseLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNewHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		format  string
		wantErr error
	}{
		{"text", nil},
		{"TEXT", nil},
		{"Text", nil},
		{"json", nil},
		{"JSON", nil},
		{"Json", nil},
		{"xml", ErrInvalidFormat},
		{"", ErrInvalidFormat},
	}

	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			t.Parallel()

			handler, err := newHandler(tt.format, slog.LevelInfo)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("newHandler(%q) error = %v, want %v", tt.format, err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Errorf("newHandler(%q) unexpected error: %v", tt.format, err)
			}
			if handler == nil {
				t.Errorf("newHandler(%q) returned nil handler", tt.format)
			}
		})
	}
}
