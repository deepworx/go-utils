package inbox_test

import (
	"context"
	"sync"
	"testing"

	"github.com/deepworx/go-utils/pkg/inbox"
)

func TestDuplicateInsertIsHandledOnce(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	msg := inbox.Message{Source: "stripe", Key: "evt_1", Type: "charge.succeeded", Payload: []byte(`{"n":1}`)}
	for i, want := range []bool{true, false} {
		dup := msg
		dup.Payload = []byte(`{"n":2}`)
		if i == 0 {
			dup = msg
		}
		inserted, err := store.Insert(t.Context(), dup)
		if err != nil {
			t.Fatalf("Insert #%d error = %v", i+1, err)
		}
		if inserted != want {
			t.Fatalf("Insert #%d inserted = %v, want %v", i+1, inserted, want)
		}
	}

	rec := &recorder{}
	drain(t, newWorker(t, store, inbox.Config{Source: "stripe", Handle: rec.handle(nil)}))

	got := rec.deliveries()
	if len(got) != 1 {
		t.Fatalf("handled %d deliveries, want 1", len(got))
	}
	if string(got[0].Payload) != `{"n":1}` || got[0].Key != "evt_1" || got[0].Attempt != 1 {
		t.Errorf("delivery = %+v, want first payload, key evt_1, attempt 1", got[0])
	}
}

// recorder records deliveries and answers each with result(delivery).
type recorder struct {
	mu  sync.Mutex
	got []inbox.Delivery
}

func (r *recorder) handle(result func(inbox.Delivery) error) inbox.Handler {
	return func(_ context.Context, d inbox.Delivery) error {
		r.mu.Lock()
		r.got = append(r.got, d)
		r.mu.Unlock()
		if result == nil {
			return nil
		}
		return result(d)
	}
}

func (r *recorder) deliveries() []inbox.Delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]inbox.Delivery(nil), r.got...)
}

func newWorker(t *testing.T, store *inbox.Store, cfg inbox.Config, opts ...inbox.Option) *inbox.Worker {
	t.Helper()
	w, err := inbox.NewWorker(store, cfg, opts...)
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}
	return w
}

func drain(t *testing.T, w *inbox.Worker) {
	t.Helper()
	if err := w.Drain(t.Context()); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
}
