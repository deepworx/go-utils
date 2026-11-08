package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepworx/go-utils/internal/quickstart/todo"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

func TestWriteError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "no title",
			err:        fmt.Errorf("open task: %w", todo.ErrNoTitle),
			wantStatus: http.StatusBadRequest,
			wantBody:   "open task: todo: task needs a title\n",
		},
		{
			name:       "not found",
			err:        fmt.Errorf("complete task: %w", eventstore.ErrNotFound),
			wantStatus: http.StatusNotFound,
			wantBody:   "complete task: eventstore: stream not found\n",
		},
		{
			name:       "already done",
			err:        fmt.Errorf("complete task: %w", todo.ErrAlreadyDone),
			wantStatus: http.StatusConflict,
			wantBody:   "complete task: todo: task already done\n",
		},
		{
			name:       "concurrency conflict",
			err:        fmt.Errorf("complete task: %w", eventstore.ErrConcurrencyConflict),
			wantStatus: http.StatusConflict,
			wantBody:   "complete task: eventstore: concurrency conflict\n",
		},
		{
			name:       "client gone",
			err:        fmt.Errorf("list tasks: %w", context.Canceled),
			wantStatus: 499,
			wantBody:   "list tasks: context canceled\n",
		},
		{
			name:       "unmapped error hides details",
			err:        errors.New("db password leaked"),
			wantStatus: http.StatusInternalServerError,
			wantBody:   "internal error\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			writeError(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/tasks", nil), tt.err)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}
