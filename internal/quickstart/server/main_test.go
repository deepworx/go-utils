package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/internal/quickstart/app"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

// TestPostgresEndToEnd runs the quickstart's Postgres main against a
// database migrated the way the quickstart describes, with the library
// tables in the non-public schema "todo". Pg tests in this package must not
// run in parallel: pg_snapshot_xmin is cluster-wide.
func TestPostgresEndToEnd(t *testing.T) {
	pool := pgtest.New(t)
	migrate(t, pool, "todo")
	mailer := &recordingMailer{}

	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, stop := context.WithCancel(t.Context())
	var g errgroup.Group
	g.Go(func() error {
		return serve(ctx, serveInput{Pool: pool, Schema: "todo", Listener: ln, Mailer: mailer, Tenant: "default"})
	})
	t.Cleanup(func() {
		stop()
		if err := g.Wait(); err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	c := client{t: t, base: "http://" + ln.Addr().String()}

	var opened struct {
		ID       string `json:"id"`
		Position string `json:"position"`
	}
	c.do(http.MethodPost, "/tasks", `{"title":"Write quickstart","assignee":"alice@example.com"}`, http.StatusCreated, &opened)

	list := c.list(opened.Position)
	if list.Freshness != "Fresh" {
		t.Fatalf("freshness = %q, want Fresh", list.Freshness)
	}
	if len(list.Tasks) != 1 || list.Tasks[0].ID.String() != opened.ID || list.Tasks[0].Title != "Write quickstart" {
		t.Fatalf("tasks = %+v, want the opened task", list.Tasks)
	}

	waitFor(t, "reactor follow-up command", func() bool {
		tasks := c.list("").Tasks
		return len(tasks) == 1 && tasks[0].Notified
	})
	if got := mailer.sent(); len(got) != 1 || got[0].To != "alice@example.com" {
		t.Fatalf("mails = %+v, want one to alice", got)
	}

	c.do(http.MethodPost, "/webhooks/tracker", `{"delivery_id":"d-1","task_id":"`+opened.ID+`"}`, http.StatusAccepted, nil)
	waitFor(t, "inbox worker command", func() bool {
		tasks := c.list("").Tasks
		return len(tasks) == 1 && tasks[0].Done
	})

	var inSchema, inPublic int
	err = pool.QueryRow(t.Context(),
		`SELECT (SELECT count(*) FROM todo.events), (SELECT count(*) FROM public.events)`).Scan(&inSchema, &inPublic)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if inSchema != 3 || inPublic != 0 {
		t.Fatalf("events in todo = %d, in public = %d; want 3 and 0", inSchema, inPublic)
	}
}

// migrate applies the migrations the quickstart tells a service to write:
// the library schema copied into the given schema, then the service's own.
func migrate(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	library, err := os.ReadFile(filepath.Join(dir, "..", "..", "..", "docs", "postgres", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	service, err := os.ReadFile(filepath.Join(dir, "..", "migrations", "000002_task_list.up.sql"))
	if err != nil {
		t.Fatalf("read service migration: %v", err)
	}
	ddl := "CREATE SCHEMA " + schema + ";\n" + strings.ReplaceAll(string(library), "public.", schema+".")
	for _, sql := range []string{ddl, string(service)} {
		if _, err := pool.Exec(t.Context(), sql); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
}

type client struct {
	t    *testing.T
	base string
}

type taskList struct {
	Tasks     []app.Task `json:"tasks"`
	Freshness string     `json:"freshness"`
}

func (c client) list(after string) taskList {
	var l taskList
	c.do(http.MethodGet, "/tasks?after="+after, "", http.StatusOK, &l)
	return l
}

func (c client) do(method, path, body string, wantStatus int, out any) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.base+path, bytes.NewBufferString(body))
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-User", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		c.t.Fatalf("%s %s: status %d, want %d", method, path, resp.StatusCode, wantStatus)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			c.t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
}

type recordingMailer struct {
	mu    sync.Mutex
	mails []app.Mail
}

func (m *recordingMailer) Send(_ context.Context, mail app.Mail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mails = append(m.mails, mail)
	return nil
}

func (m *recordingMailer) sent() []app.Mail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]app.Mail(nil), m.mails...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
