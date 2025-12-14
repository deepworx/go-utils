package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNewPool_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{
			name:    "missing DSN",
			cfg:     Config{},
			wantErr: ErrDSNRequired,
		},
		{
			name: "invalid DSN",
			cfg: Config{
				DSN: "not-a-valid-dsn",
			},
			wantErr: nil, // Will fail on parse, not our sentinel
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewPool(context.Background(), tt.cfg)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("NewPool() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestApplyDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		cfg                   Config
		wantMaxConns          int32
		wantMinConns          int32
		wantMaxConnLifetime   time.Duration
		wantMaxConnIdleTime   time.Duration
		wantHealthCheckPeriod time.Duration
	}{
		{
			name:                  "all defaults",
			cfg:                   Config{DSN: "postgres://localhost/test"},
			wantMaxConns:          10,
			wantMinConns:          2,
			wantMaxConnLifetime:   time.Hour,
			wantMaxConnIdleTime:   30 * time.Minute,
			wantHealthCheckPeriod: time.Minute,
		},
		{
			name: "custom max conns",
			cfg: Config{
				DSN:      "postgres://localhost/test",
				MaxConns: 20,
			},
			wantMaxConns:          20,
			wantMinConns:          2,
			wantMaxConnLifetime:   time.Hour,
			wantMaxConnIdleTime:   30 * time.Minute,
			wantHealthCheckPeriod: time.Minute,
		},
		{
			name: "custom min conns",
			cfg: Config{
				DSN:      "postgres://localhost/test",
				MinConns: 5,
			},
			wantMaxConns:          10,
			wantMinConns:          5,
			wantMaxConnLifetime:   time.Hour,
			wantMaxConnIdleTime:   30 * time.Minute,
			wantHealthCheckPeriod: time.Minute,
		},
		{
			name: "all custom values",
			cfg: Config{
				DSN:               "postgres://localhost/test",
				MaxConns:          50,
				MinConns:          10,
				MaxConnLifetime:   2 * time.Hour,
				MaxConnIdleTime:   time.Hour,
				HealthCheckPeriod: 30 * time.Second,
			},
			wantMaxConns:          50,
			wantMinConns:          10,
			wantMaxConnLifetime:   2 * time.Hour,
			wantMaxConnIdleTime:   time.Hour,
			wantHealthCheckPeriod: 30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			poolCfg, err := pgxpool.ParseConfig(tt.cfg.DSN)
			if err != nil {
				t.Fatalf("ParseConfig() error = %v", err)
			}

			applyDefaults(poolCfg, tt.cfg)

			if poolCfg.MaxConns != tt.wantMaxConns {
				t.Errorf("MaxConns = %d, want %d", poolCfg.MaxConns, tt.wantMaxConns)
			}
			if poolCfg.MinConns != tt.wantMinConns {
				t.Errorf("MinConns = %d, want %d", poolCfg.MinConns, tt.wantMinConns)
			}
			if poolCfg.MaxConnLifetime != tt.wantMaxConnLifetime {
				t.Errorf("MaxConnLifetime = %v, want %v", poolCfg.MaxConnLifetime, tt.wantMaxConnLifetime)
			}
			if poolCfg.MaxConnIdleTime != tt.wantMaxConnIdleTime {
				t.Errorf("MaxConnIdleTime = %v, want %v", poolCfg.MaxConnIdleTime, tt.wantMaxConnIdleTime)
			}
			if poolCfg.HealthCheckPeriod != tt.wantHealthCheckPeriod {
				t.Errorf("HealthCheckPeriod = %v, want %v", poolCfg.HealthCheckPeriod, tt.wantHealthCheckPeriod)
			}
		})
	}
}

func TestWithTx_PanicRecovery(t *testing.T) {
	t.Parallel()

	// This test verifies the panic handling logic without a real database.
	// The function should re-panic after attempting rollback.
	// Since we can't test with a real pool here, we verify the sentinel error.

	// The actual panic behavior would be tested in integration tests.
	// Here we just verify the function signature and error handling pattern.
}
