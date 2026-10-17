package eventstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/internal/eventstoretest"
	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/guard"
	"github.com/deepworx/go-utils/pkg/postgres/numbering"
)

func TestInMemoryContract(t *testing.T) {
	t.Parallel()
	eventstoretest.Run(t, func(*testing.T) eventstoretest.Backend {
		var base *eventstore.InMemoryStore
		return eventstoretest.Backend{
			Store: func(cfg eventstoretest.StoreConfig) eventstore.Store {
				s := eventstore.NewInMemoryStore(cfg.Codec, eventstore.WithClock(cfg.Clock))
				if base == nil {
					base = s
				} else {
					eventstore.ShareLog(s, base)
				}
				return s
			},
			UnitOfWork: eventstore.NewInMemoryUnitOfWork(),
			Guard:      guard.NewInMemoryStore(),
			Numbering:  numbering.NewInMemoryStore(),
			Outer:      postgres.NewInMemoryUnitOfWork().Execute,
		}
	})
}

func TestInMemoryUnitOfWorkLockHonorsContext(t *testing.T) {
	t.Parallel()
	uow := eventstore.NewInMemoryUnitOfWork()
	ctx := ctxutil.WithTenantID(context.Background(), "t1")
	stream := uuid.New()

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- uow.Execute(ctx, stream, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	t.Cleanup(func() {
		close(release)
		if err := <-done; err != nil {
			t.Errorf("holder Execute: %v", err)
		}
	})

	tests := []struct {
		name   string
		ctx    context.Context
		stream uuid.UUID
		want   error
	}{
		{"same stream waits until ctx ends", timeoutCtx(t, ctx), stream, context.DeadlineExceeded},
		{"other tenant does not wait", ctxutil.WithTenantID(ctx, "t2"), stream, nil},
		{"other stream does not wait", ctx, uuid.New(), nil},
	}
	for _, tt := range tests {
		err := uow.Execute(tt.ctx, tt.stream, func(context.Context) error { return nil })
		if !errors.Is(err, tt.want) {
			t.Fatalf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func timeoutCtx(t *testing.T, parent context.Context) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 20*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}
