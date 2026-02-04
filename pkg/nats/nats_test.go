package nats

import (
	"errors"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{
			name:    "missing URL",
			cfg:     Config{},
			wantErr: ErrURLRequired,
		},
		{
			name: "valid minimal config",
			cfg: Config{
				URL: "nats://localhost:4222",
			},
			wantErr: nil,
		},
		{
			name: "valid with TLS CA only",
			cfg: Config{
				URL: "nats://localhost:4222",
				TLS: &TLSConfig{
					CAFile: "/path/to/ca.crt",
				},
			},
			wantErr: nil,
		},
		{
			name: "valid mTLS config",
			cfg: Config{
				URL: "nats://localhost:4222",
				TLS: &TLSConfig{
					CAFile:   "/path/to/ca.crt",
					CertFile: "/path/to/client.crt",
					KeyFile:  "/path/to/client.key",
				},
			},
			wantErr: nil,
		},
		{
			name: "incomplete mTLS - cert without key",
			cfg: Config{
				URL: "nats://localhost:4222",
				TLS: &TLSConfig{
					CertFile: "/path/to/client.crt",
				},
			},
			wantErr: ErrIncompleteMTLS,
		},
		{
			name: "incomplete mTLS - key without cert",
			cfg: Config{
				URL: "nats://localhost:4222",
				TLS: &TLSConfig{
					KeyFile: "/path/to/client.key",
				},
			},
			wantErr: ErrIncompleteMTLS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cfg.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("Validate() unexpected error = %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	if cfg.MaxReconnects != -1 {
		t.Errorf("MaxReconnects = %d, want -1", cfg.MaxReconnects)
	}
	if cfg.ReconnectWait != 2*time.Second {
		t.Errorf("ReconnectWait = %v, want %v", cfg.ReconnectWait, 2*time.Second)
	}
	if cfg.ReconnectJitter != 100*time.Millisecond {
		t.Errorf("ReconnectJitter = %v, want %v", cfg.ReconnectJitter, 100*time.Millisecond)
	}
	if cfg.ConnectTimeout != 5*time.Second {
		t.Errorf("ConnectTimeout = %v, want %v", cfg.ConnectTimeout, 5*time.Second)
	}
	if cfg.DrainTimeout != 30*time.Second {
		t.Errorf("DrainTimeout = %v, want %v", cfg.DrainTimeout, 30*time.Second)
	}
	if cfg.TLS != nil {
		t.Errorf("TLS = %v, want nil", cfg.TLS)
	}
}

func TestConnect_ValidationError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{
			name:    "missing URL",
			cfg:     Config{},
			wantErr: ErrURLRequired,
		},
		{
			name: "incomplete mTLS",
			cfg: Config{
				URL: "nats://localhost:4222",
				TLS: &TLSConfig{
					CertFile: "/path/to/client.crt",
				},
			},
			wantErr: ErrIncompleteMTLS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Connect(t.Context(), tt.cfg)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Connect() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestBuildOptions(t *testing.T) {
	t.Parallel()

	cfg := Config{
		URL:             "nats://localhost:4222",
		MaxReconnects:   10,
		ReconnectWait:   time.Second,
		ReconnectJitter: 50 * time.Millisecond,
		ConnectTimeout:  3 * time.Second,
	}

	opts := buildOptions(cfg)
	if len(opts) == 0 {
		t.Error("buildOptions() returned empty slice")
	}
}

func TestBuildTLSOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		tlsCfg       *TLSConfig
		wantOptCount int
	}{
		{
			name:         "nil config",
			tlsCfg:       nil,
			wantOptCount: 0,
		},
		{
			name:         "CA only",
			tlsCfg:       &TLSConfig{CAFile: "/path/to/ca.crt"},
			wantOptCount: 1,
		},
		{
			name: "mTLS",
			tlsCfg: &TLSConfig{
				CAFile:   "/path/to/ca.crt",
				CertFile: "/path/to/client.crt",
				KeyFile:  "/path/to/client.key",
			},
			wantOptCount: 2,
		},
		{
			name: "skip verify only",
			tlsCfg: &TLSConfig{
				SkipVerify: true,
			},
			wantOptCount: 1,
		},
		{
			name: "full config",
			tlsCfg: &TLSConfig{
				CAFile:     "/path/to/ca.crt",
				CertFile:   "/path/to/client.crt",
				KeyFile:    "/path/to/client.key",
				SkipVerify: true,
			},
			wantOptCount: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.tlsCfg == nil {
				return
			}
			opts := buildTLSOptions(tt.tlsCfg)
			if len(opts) != tt.wantOptCount {
				t.Errorf("buildTLSOptions() returned %d options, want %d", len(opts), tt.wantOptCount)
			}
		})
	}
}

func TestHeaderCarrier(t *testing.T) {
	t.Parallel()

	carrier := make(headerCarrier)
	carrier.Set("traceparent", "00-abc123-def456-01")
	carrier.Set("tracestate", "foo=bar")

	if got := carrier.Get("traceparent"); got != "00-abc123-def456-01" {
		t.Errorf("Get(traceparent) = %q, want %q", got, "00-abc123-def456-01")
	}

	keys := carrier.Keys()
	if len(keys) != 2 {
		t.Errorf("Keys() returned %d keys, want 2", len(keys))
	}
}
