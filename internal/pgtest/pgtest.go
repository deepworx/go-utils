// Package pgtest runs Postgres-backed tests against one shared testcontainers
// Postgres per test binary.
//
// Wire it up in TestMain and request a database per test:
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }
//
//	func TestSomething(t *testing.T) {
//		pool := pgtest.New(t)
//		...
//	}
//
// The documented docs/postgres/schema.sql is loaded once into a template
// database; every New call clones it with CREATE DATABASE … TEMPLATE.
// Postgres tests run by default, are skipped with -short, and fail when
// Docker is unavailable.
package pgtest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// ImageEnv names the environment variable that overrides the Postgres image.
const ImageEnv = "GOUTILS_TEST_PG_IMAGE"

// DefaultImage is the Postgres image used when ImageEnv is unset.
const DefaultImage = "postgres:18"

const (
	templateDatabase = "pgtest_template"
	startupTimeout   = 3 * time.Minute
	cleanupTimeout   = 30 * time.Second
)

var errNotStarted = errors.New("pgtest: Run was not called from TestMain")

type harness struct {
	admin    *pgxpool.Pool
	baseConf *pgxpool.Config
	// createMu serializes CREATE DATABASE … TEMPLATE, which fails when the
	// template is copied concurrently.
	createMu sync.Mutex
	seq      atomic.Int64
}

var shared *harness

type runConfig struct {
	image       string
	schemaFiles []string
}

// Run starts the shared Postgres container, loads schema.sql into the
// template database, runs the tests and tears everything down. It returns the
// exit code for os.Exit. With -short it runs the tests without Postgres.
func Run(m *testing.M) int {
	return run(m, runConfig{schemaFiles: []string{schemaPath()}})
}

func run(m *testing.M, cfg runConfig) int {
	if !flag.Parsed() {
		flag.Parse()
	}
	if testing.Short() {
		return m.Run()
	}
	if cfg.image == "" {
		cfg.image = image()
	}

	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	ctr, h, err := start(ctx, cfg)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: %v\n", err)
		return 1
	}
	shared = h
	defer func() {
		h.admin.Close()
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest: terminate container: %v\n", err)
		}
	}()
	return m.Run()
}

// New returns a pool connected to a fresh database cloned from the schema
// template. The pool is closed and the database dropped when the test ends.
// It skips the test under -short.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("pgtest: Postgres tests are skipped with -short")
	}
	if shared == nil {
		t.Fatal(errNotStarted)
	}
	pool, err := shared.newDatabase(t)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	return pool
}

func (h *harness) newDatabase(t testing.TB) (*pgxpool.Pool, error) {
	ctx := t.Context()
	name := fmt.Sprintf("pgtest_%d", h.seq.Add(1))

	h.createMu.Lock()
	_, err := h.admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDatabase))
	h.createMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("create database %s: %w", name, err)
	}

	conf := h.baseConf.Copy()
	conf.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", name, err)
	}

	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if _, err := h.admin.Exec(ctx, fmt.Sprintf("DROP DATABASE %s WITH (FORCE)", name)); err != nil {
			t.Errorf("pgtest: drop database %s: %v", name, err)
		}
	})
	return pool, nil
}

func start(ctx context.Context, cfg runConfig) (*postgres.PostgresContainer, *harness, error) {
	ctr, err := startContainer(ctx, cfg.image)
	if err != nil {
		return nil, nil, err
	}
	h, err := newHarness(ctx, ctr, cfg.schemaFiles)
	if err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		return nil, nil, err
	}
	return ctr, h, nil
}

func startContainer(ctx context.Context, img string) (ctr *postgres.PostgresContainer, err error) {
	// testcontainers panics instead of returning an error on some Docker
	// discovery failures.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
		if err != nil {
			err = fmt.Errorf("start Postgres container %s (Docker is required; run with -short to skip Postgres tests): %w", img, err)
		}
	}()
	return postgres.Run(ctx, img,
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		postgres.BasicWaitStrategies(),
	)
}

func newHarness(ctx context.Context, ctr *postgres.PostgresContainer, schemaFiles []string) (*harness, error) {
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("container connection string: %w", err)
	}
	baseConf, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, baseConf.Copy())
	if err != nil {
		return nil, fmt.Errorf("connect admin pool: %w", err)
	}
	if err := buildTemplate(ctx, admin, baseConf, schemaFiles); err != nil {
		admin.Close()
		return nil, err
	}
	return &harness{admin: admin, baseConf: baseConf}, nil
}

func buildTemplate(ctx context.Context, admin *pgxpool.Pool, baseConf *pgxpool.Config, schemaFiles []string) error {
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateDatabase); err != nil {
		return fmt.Errorf("create template database: %w", err)
	}

	connConf := baseConf.ConnConfig.Copy()
	connConf.Database = templateDatabase
	conn, err := pgx.ConnectConfig(ctx, connConf)
	if err != nil {
		return fmt.Errorf("connect template database: %w", err)
	}
	defer conn.Close(ctx)

	for _, path := range schemaFiles {
		if err := applySQLFile(ctx, conn, path); err != nil {
			return err
		}
	}
	return nil
}

func applySQLFile(ctx context.Context, conn *pgx.Conn, path string) error {
	sql, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		return fmt.Errorf("apply %s: %w", path, err)
	}
	return nil
}

func image() string {
	if img := os.Getenv(ImageEnv); img != "" {
		return img
	}
	return DefaultImage
}

func schemaPath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "docs", "postgres", "schema.sql")
}
