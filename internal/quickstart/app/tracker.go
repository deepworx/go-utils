package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/quickstart/todo"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/inbox"
)

// TrackerSource is the Inbox Source of the issue tracker's webhooks.
const TrackerSource = "tracker"

// TrackerWebhook is the body of a tracker webhook: a task was closed in
// the tracker.
type TrackerWebhook struct {
	DeliveryID string    `json:"delivery_id"`
	TaskID     uuid.UUID `json:"task_id"`
}

// HandleTrackerWebhook is the inbox.Handler of TrackerSource: it completes
// the Task the webhook names.
func (a *App) HandleTrackerWebhook(ctx context.Context, d inbox.Delivery) error {
	var hook TrackerWebhook
	if err := json.Unmarshal(d.Payload, &hook); err != nil {
		return inbox.Permanent(fmt.Errorf("decode tracker webhook: %w", err))
	}
	_, err := a.CompleteTask(ctx, todo.TaskID(hook.TaskID))
	switch {
	case errors.Is(err, todo.ErrAlreadyDone):
		return inbox.Ignore("task already done")
	case errors.Is(err, eventstore.ErrNotFound):
		return inbox.Permanent(err)
	}
	return err
}

// ReceiveTrackerWebhook stores a tracker webhook in the Inbox and answers
// 202 at once; the Worker of TrackerSource handles it. A redelivered
// webhook is deduplicated by its delivery ID.
func ReceiveTrackerWebhook(store *inbox.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		var hook TrackerWebhook
		if err := json.Unmarshal(body, &hook); err != nil || hook.DeliveryID == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		tenant, _ := ctxutil.TenantID(r.Context())
		_, err = store.Insert(r.Context(), inbox.Message{
			Source:  TrackerSource,
			Key:     hook.DeliveryID,
			Type:    "task.closed",
			Tenant:  tenant,
			Payload: body,
		})
		if err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
}
