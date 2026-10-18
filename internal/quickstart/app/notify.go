package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/deepworx/go-utils/internal/quickstart/todo"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// ErrInvalidAddress is returned by a Mailer for an address it can never
// deliver to.
var ErrInvalidAddress = errors.New("app: invalid mail address")

// Mail is one outbound mail.
type Mail struct {
	To      string
	Subject string
	// IdempotencyKey lets the mail provider drop a repeated send.
	IdempotencyKey string
}

// Mailer sends mails.
type Mailer interface {
	Send(ctx context.Context, m Mail) error
}

// NotifyAssignee builds the handler of the notify_assignee Reactor: it
// mails the assignee of an opened Task, then records that with a follow-up
// command. Delivery is at least once, so the mail carries the event ID as
// its idempotency key and MarkNotified is idempotent.
func NotifyAssignee(codec *eventstore.Codec, tasks *Tasks, mailer Mailer) *consumer.Router {
	r := consumer.NewRouter(codec)
	consumer.On(r, func(ctx context.Context, env eventstore.Envelope, e todo.TaskOpened) error {
		if e.Assignee == "" {
			return nil
		}
		err := mailer.Send(ctx, Mail{
			To:             e.Assignee,
			Subject:        "New task: " + e.Title,
			IdempotencyKey: env.EventID.String(),
		})
		if errors.Is(err, ErrInvalidAddress) {
			return consumer.Permanent(err)
		}
		if err != nil {
			return fmt.Errorf("mail assignee: %w", err)
		}
		_, err = tasks.Update(ctx, todo.TaskID(env.StreamID), func(_ context.Context, t *todo.Task) error {
			t.MarkNotified()
			return nil
		})
		return err
	})
	return r
}
