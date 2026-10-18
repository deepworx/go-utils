package projection_test

import (
	"context"
	"testing"
	"time"

	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/eventsourcing/projection"
)

// A Subscription whose ctx ends closes C without a Close call.
func TestSubscriptionEndsWithContext(t *testing.T) {
	t.Parallel()
	store := eventstore.NewInMemoryStore(eventstore.NewCodec())
	rt := consumer.NewInMemoryRuntime(consumer.InMemoryRuntimeConfig{Store: store})
	ctx, cancel := context.WithCancel(t.Context())
	sub, err := projection.Subscribe(ctx, rt)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	cancel()
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("received a Change, want C closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("C still open after ctx ended")
	}
}
