package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/quickstart/todo"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// Metadata puts the request metadata into the request ctx: the fixed
// Tenant of a single-tenant service, the Actor and the request ID, which
// becomes the Correlation of the events the request writes (ADR-0010).
//
// The X-User header stands in for real authentication. With
// pkg/connectrpc's jwtauth interceptor, Claims.UserID is the Actor and
// Claims.TenantID the Tenant without any of this.
func Metadata(tenant string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get("X-User")
		if user == "" {
			http.Error(w, "missing X-User", http.StatusUnauthorized)
			return
		}
		ctx := ctxutil.WithTenantID(r.Context(), tenant)
		ctx = ctxutil.WithActor(ctx, "user:"+user)
		if id := r.Header.Get("X-Request-ID"); id != "" {
			ctx = ctxutil.WithRequestID(ctx, id)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Routes registers the App's HTTP API on mux.
func (a *App) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /tasks", a.handleOpenTask)
	mux.HandleFunc("POST /tasks/{id}/complete", a.handleCompleteTask)
	mux.HandleFunc("GET /tasks", a.handleListTasks)
}

func (a *App) handleOpenTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title    string `json:"title"`
		Assignee string `json:"assignee"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	id, res, err := a.OpenTask(r.Context(), OpenTask{Title: in.Title, Assignee: in.Assignee})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"id":       uuid.UUID(id).String(),
		"position": FormatPosition(res.Position),
	})
}

func (a *App) handleCompleteTask(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid task id", http.StatusBadRequest)
		return
	}
	res, err := a.CompleteTask(r.Context(), todo.TaskID(id))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"position": FormatPosition(res.Position)})
}

// handleListTasks serves read-your-writes: the client passes the position
// of its last command as ?after=.
func (a *App) handleListTasks(w http.ResponseWriter, r *http.Request) {
	after, err := ParsePosition(r.URL.Query().Get("after"))
	if err != nil {
		http.Error(w, "invalid after", http.StatusBadRequest)
		return
	}
	tasks, fresh, err := a.ListTasks(r.Context(), after)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "freshness": fresh.String()})
}

// FormatPosition encodes a CommandResult.Position for an API client.
func FormatPosition(c eventstore.Cursor) string {
	return fmt.Sprintf("%d.%d", c.TransactionID, c.Position)
}

// ParsePosition decodes a position from FormatPosition. The empty string
// is the zero Cursor.
func ParsePosition(s string) (eventstore.Cursor, error) {
	if s == "" {
		return eventstore.Cursor{}, nil
	}
	tx, pos, ok := strings.Cut(s, ".")
	if !ok {
		return eventstore.Cursor{}, fmt.Errorf("position %q: missing '.'", s)
	}
	var c eventstore.Cursor
	var err error
	if c.TransactionID, err = strconv.ParseUint(tx, 10, 64); err != nil {
		return eventstore.Cursor{}, fmt.Errorf("position %q: %w", s, err)
	}
	if c.Position, err = strconv.ParseInt(pos, 10, 64); err != nil {
		return eventstore.Cursor{}, fmt.Errorf("position %q: %w", s, err)
	}
	return c, nil
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, todo.ErrNoTitle):
		http.Error(w, todo.ErrNoTitle.Error(), http.StatusBadRequest)
	case errors.Is(err, eventstore.ErrNotFound):
		http.Error(w, "task not found", http.StatusNotFound)
	case errors.Is(err, todo.ErrAlreadyDone):
		http.Error(w, todo.ErrAlreadyDone.Error(), http.StatusConflict)
	case errors.Is(err, eventstore.ErrConcurrencyConflict):
		http.Error(w, "task changed concurrently, retry", http.StatusConflict)
	default:
		slog.ErrorContext(r.Context(), "request failed", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
