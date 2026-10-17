package reactor

import (
	"context"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// Redeliver hands env to r's live handler once more, as the Consumer does
// after a crash between handling env and advancing the Cursor.
func Redeliver(ctx context.Context, r *Reactor, env eventstore.Envelope) error {
	ctx = ctxutil.WithTenantID(ctx, env.TenantID)
	ctx = ctxutil.WithCorrelationID(ctx, env.CorrelationID)
	return r.handle(ctx, env)
}
