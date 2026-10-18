package consumer_test

import (
	"testing"

	"github.com/deepworx/go-utils/internal/consumertest"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

func TestInMemoryContract(t *testing.T) {
	t.Parallel()
	consumertest.Run(t, func(_ *testing.T, codec *eventstore.Codec) consumertest.Backend {
		store := eventstore.NewInMemoryStore(codec)
		rt := consumer.NewInMemoryRuntime(consumer.InMemoryRuntimeConfig{Store: store})
		return consumertest.Backend{
			Store:      store,
			UnitOfWork: eventstore.NewInMemoryUnitOfWork(),
			Runtime:    rt,
			Effects:    &consumertest.InMemoryEffects{},
			Restart: func(*testing.T) consumer.Runtime {
				rt = consumer.RestartInMemory(rt)
				return rt
			},
		}
	}, consumertest.InBubble())
}
