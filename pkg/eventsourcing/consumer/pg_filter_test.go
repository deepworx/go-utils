package consumer_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
)

// TestPgFilterHistoryUsesTypeIndex checks that the claim-time check for
// added types reads one index range per type, even under a generic plan.
func TestPgFilterHistoryUsesTypeIndex(t *testing.T) {
	e := newPgEnv(t, pgEnvConfig{})
	for range 50 {
		e.write(t, noteWritten{Text: "x"}, noteDeleted{})
	}
	ctx := t.Context()
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, stmt := range []string{
		"ANALYZE public.events",
		"SET plan_cache_mode = force_generic_plan",
		"SET enable_seqscan = off",
		"PREPARE filter_history AS " + consumer.PgFilterHistorySQL("public"),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	rows, err := conn.Query(ctx, `EXPLAIN EXECUTE filter_history('{Test.Note.Written,Test.Note.Deleted}', '1000000', 100)`)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	text := strings.Join(plan, "\n")

	if regexp.MustCompile(`Filter: .*event_type`).MatchString(text) {
		t.Errorf("plan filters on event_type:\n%s", text)
	}
	if !regexp.MustCompile(`Index Cond: .*event_type = t\.event_type`).MatchString(text) {
		t.Errorf("plan has no per-type index range:\n%s", text)
	}
}
