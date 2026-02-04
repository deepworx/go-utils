// Package nats provides NATS and JetStream connection utilities with tracing integration.
//
// It offers connection initialization with TLS/mTLS support, health checks,
// graceful shutdown, and distributed tracing via OpenTelemetry.
package nats

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/deepworx/go-utils/pkg/shutdown"
	"github.com/nats-io/nats.go"
)

// Config holds configuration for NATS connection.
type Config struct {
	// URL is the NATS server URL.
	// Required. Example: "nats://localhost:4222"
	URL string `koanf:"url"`

	// MaxReconnects is the maximum number of reconnection attempts.
	// -1 means unlimited reconnects.
	MaxReconnects int `koanf:"max_reconnects"`

	// ReconnectWait is the time to wait between reconnection attempts.
	ReconnectWait time.Duration `koanf:"reconnect_wait"`

	// ReconnectJitter is the maximum random jitter added to reconnect wait.
	ReconnectJitter time.Duration `koanf:"reconnect_jitter"`

	// ConnectTimeout is the timeout for initial connection.
	ConnectTimeout time.Duration `koanf:"connect_timeout"`

	// DrainTimeout is the timeout for graceful drain during shutdown.
	DrainTimeout time.Duration `koanf:"drain_timeout"`

	// TLS holds TLS/mTLS configuration. Optional.
	TLS *TLSConfig `koanf:"tls"`
}

// TLSConfig holds TLS and mTLS configuration.
type TLSConfig struct {
	// CAFile is the path to the CA certificate for server verification.
	CAFile string `koanf:"ca_file"`

	// SkipVerify disables server certificate verification.
	// Default: false (verification enabled).
	SkipVerify bool `koanf:"skip_verify"`

	// CertFile is the path to the client certificate for mTLS.
	CertFile string `koanf:"cert_file"`

	// KeyFile is the path to the client private key for mTLS.
	KeyFile string `koanf:"key_file"`
}

// DefaultConfig returns a Config with sensible default values.
// URL is required and must be set by the caller.
func DefaultConfig() Config {
	return Config{
		MaxReconnects:   -1,
		ReconnectWait:   2 * time.Second,
		ReconnectJitter: 100 * time.Millisecond,
		ConnectTimeout:  5 * time.Second,
		DrainTimeout:    30 * time.Second,
	}
}

// Validate checks the configuration for errors.
func (c Config) Validate() error {
	if c.URL == "" {
		return ErrURLRequired
	}
	if c.TLS != nil {
		hasCert := c.TLS.CertFile != ""
		hasKey := c.TLS.KeyFile != ""
		if hasCert != hasKey {
			return fmt.Errorf("validate tls: %w", ErrIncompleteMTLS)
		}
	}
	return nil
}

// Connect establishes a connection to the NATS server.
// It registers a shutdown handler to drain the connection gracefully.
func Connect(ctx context.Context, cfg Config) (*nats.Conn, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	opts := buildOptions(cfg)

	if hostname, err := os.Hostname(); err == nil {
		opts = append(opts, nats.Name(hostname))
	}

	conn, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", cfg.URL, err)
	}

	drainTimeout := cfg.DrainTimeout
	if drainTimeout == 0 {
		drainTimeout = 30 * time.Second
	}

	shutdown.Register(func(ctx context.Context) error {
		return drainWithContext(ctx, conn, drainTimeout)
	})

	if err := registerMetrics(conn); err != nil {
		slog.Warn("failed to register nats metrics", "error", err)
	}

	return conn, nil
}

func buildOptions(cfg Config) []nats.Option {
	opts := []nats.Option{
		nats.Timeout(cfg.ConnectTimeout),
		nats.MaxReconnects(cfg.MaxReconnects),
		nats.ReconnectWait(cfg.ReconnectWait),
		nats.ReconnectJitter(cfg.ReconnectJitter, cfg.ReconnectJitter),

		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			slog.Warn("nats disconnected",
				"url", nc.ConnectedUrl(),
				"error", err,
			)
		}),

		nats.ReconnectHandler(func(nc *nats.Conn) {
			slog.Info("nats reconnected",
				"url", nc.ConnectedUrl(),
				"reconnects", nc.Stats().Reconnects,
			)
		}),

		nats.ErrorHandler(func(nc *nats.Conn, sub *nats.Subscription, err error) {
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			slog.Error("nats error",
				"url", nc.ConnectedUrl(),
				"subject", subject,
				"error", err,
			)
		}),
	}

	if cfg.TLS != nil {
		opts = append(opts, buildTLSOptions(cfg.TLS)...)
	}

	return opts
}

func buildTLSOptions(tlsCfg *TLSConfig) []nats.Option {
	var opts []nats.Option

	if tlsCfg.CAFile != "" {
		opts = append(opts, nats.RootCAs(tlsCfg.CAFile))
	}

	if tlsCfg.CertFile != "" && tlsCfg.KeyFile != "" {
		opts = append(opts, nats.ClientCert(tlsCfg.CertFile, tlsCfg.KeyFile))
	}

	if tlsCfg.SkipVerify {
		opts = append(opts, nats.Secure(&tls.Config{InsecureSkipVerify: true})) //nolint:gosec
	}

	return opts
}

func drainWithContext(ctx context.Context, conn *nats.Conn, timeout time.Duration) error {
	if err := conn.Drain(); err != nil {
		return fmt.Errorf("start drain: %w", err)
	}

	drainCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case <-drainCtx.Done():
		conn.Close()
		return fmt.Errorf("drain timeout: %w", drainCtx.Err())
	case <-waitForDrain(conn):
		return nil
	}
}

// waitForDrain polls the connection until it is closed.
// The NATS library does not provide a callback for drain completion,
// so polling is required. The 50ms interval balances responsiveness
// against CPU usage during shutdown.
func waitForDrain(conn *nats.Conn) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for !conn.IsClosed() {
			time.Sleep(50 * time.Millisecond)
		}
		close(ch)
	}()
	return ch
}
