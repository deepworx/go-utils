package pgtest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) {
	os.Exit(run(m, runConfig{
		schemaFiles: []string{schemaPath(), filepath.Join("testdata", "marker.sql")},
	}))
}

func TestNewGivesIsolatedDatabasesFromTemplate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	first := New(t)
	second := New(t)

	if a, b := databaseName(ctx, t, first), databaseName(ctx, t, second); a == b {
		t.Fatalf("both pools use database %q, want distinct databases", a)
	}

	if _, err := first.Exec(ctx, "INSERT INTO pgtest_marker (note) VALUES ('first')"); err != nil {
		t.Fatalf("insert into first database: %v", err)
	}

	tests := []struct {
		name string
		pool *pgxpool.Pool
		want int
	}{
		{name: "first sees its own row", pool: first, want: 1},
		{name: "second is unaffected", pool: second, want: 0},
	}
	for _, tt := range tests {
		var got int
		if err := tt.pool.QueryRow(ctx, "SELECT count(*) FROM pgtest_marker").Scan(&got); err != nil {
			t.Fatalf("%s: count marker rows: %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("%s: marker rows = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestSchemaGuardRejectsOldPostgres(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("pgtest: Postgres tests are skipped with -short")
	}

	_, _, err := start(t.Context(), runConfig{
		image:       "postgres:14-alpine",
		schemaFiles: []string{schemaPath()},
	})
	if err == nil {
		t.Fatal("schema.sql applied on PostgreSQL 14, want guard to abort")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %v, want a Postgres error from the guard", err)
	}
	if !strings.Contains(pgErr.Message, "requires PostgreSQL 15 or newer") {
		t.Errorf("guard message = %q, want it to name the PostgreSQL 15 requirement", pgErr.Message)
	}
}

func databaseName(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatalf("query database name: %v", err)
	}
	return name
}
