package reactor_test

import (
	"testing"

	"github.com/deepworx/go-utils/internal/reactortest"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/reactor"
)

func TestInMemoryContract(t *testing.T) {
	t.Parallel()
	reactortest.Run(t, func(_ *testing.T, codec *eventstore.Codec) reactortest.Backend {
		store := eventstore.NewInMemoryStore(codec)
		return reactortest.Backend{
			Store:      store,
			UnitOfWork: eventstore.NewInMemoryUnitOfWork(),
			Runtime:    consumer.NewInMemoryRuntime(consumer.InMemoryRuntimeConfig{Store: store}),
			Redeliver:  reactor.Redeliver,
		}
	}, reactortest.InBubble())
}
