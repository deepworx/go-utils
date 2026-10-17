package consumertest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

func filterChangeCases(s suite) []testCase {
	return []testCase{
		{"FilterChange/RemovedTypeAccepted", s.removedTypeAccepted},
		{"FilterChange/AddedTypeWithoutHistoryAccepted", s.addedTypeWithoutHistoryAccepted},
		{"FilterChange/AddedTypeWithHistoryStopsTransactional", s.addedTypeWithHistoryStopsTransactional},
		{"FilterChange/AllTypesWithHistoryStopsTransactional", s.allTypesWithHistoryStopsTransactional},
		{"FilterChange/AddedTypeOnOwnTransactionAppliesFromCursor", s.addedTypeOnOwnTransactionAppliesFromCursor},
		{"FilterChange/AcceptedChangeNotRecheckedAfterRestart", s.acceptedChangeNotRecheckedAfterRestart},
	}
}

var (
	addedType   = added{}.EventType()
	renamedType = renamed{}.EventType()
)

// typeLog records the event types a handler saw, in order.
type typeLog struct {
	mu    sync.Mutex
	types []string
}

func (l *typeLog) handler() consumer.Handler {
	return consumer.HandlerFunc(func(_ context.Context, env eventstore.Envelope) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.types = append(l.types, env.EventType)
		return nil
	})
}

func (l *typeLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.types)
}

// release is one deployment of the Consumer "filter".
type release struct {
	mode  consumer.Mode
	types []string
}

type deployment struct {
	h  harness
	c  *consumer.Consumer
	rt consumer.Runtime
}

// deploy restarts the Runtime, as after a process restart, and creates the
// Consumer "filter" on it with r's mode and filter.
func (h harness) deploy(r release, log *typeLog) deployment {
	h.t.Helper()
	rt := h.backend.Restart(h.t)
	c, err := consumer.New(rt, consumer.Config{
		Name: "filter", Mode: r.mode, EventTypes: r.types, Handler: log.handler(),
	}, fastRetry, quiet)
	if err != nil {
		h.t.Fatalf("consumer.New: %v", err)
	}
	return deployment{h: h, c: c, rt: rt}
}

func (d deployment) drain() {
	d.h.t.Helper()
	if err := d.c.Drain(context.Background()); err != nil {
		d.h.t.Fatalf("Drain: %v", err)
	}
}

func (d deployment) status() consumer.Status {
	d.h.t.Helper()
	return d.h.status(d.c)
}

func (s suite) removedTypeAccepted(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.write(ctx, added{Name: "a"}, renamed{Name: "b"})
	var log typeLog
	h.deploy(release{consumer.Transactional, []string{addedType, renamedType}}, &log).drain()

	narrowed := h.deploy(release{consumer.Transactional, []string{addedType}}, &log)
	narrowed.drain()
	h.write(ctx, renamed{Name: "c"}, added{Name: "d"})
	narrowed.drain()

	if got, want := log.get(), []string{addedType, renamedType, addedType}; !slices.Equal(got, want) {
		t.Errorf("handled %v, want %v", got, want)
	}
	if st := narrowed.status(); st.Stopped != nil || st.Pending != 0 {
		t.Errorf("Status = %+v, want running with nothing pending", st)
	}
}

func (s suite) addedTypeWithoutHistoryAccepted(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.write(ctx, added{Name: "a"})
	var log typeLog
	h.deploy(release{consumer.Transactional, []string{addedType}}, &log).drain()

	widened := h.deploy(release{consumer.Transactional, []string{addedType, renamedType}}, &log)
	widened.drain()
	h.write(ctx, renamed{Name: "b"})
	widened.drain()

	if got, want := log.get(), []string{addedType, renamedType}; !slices.Equal(got, want) {
		t.Errorf("handled %v, want %v", got, want)
	}
}

func (s suite) addedTypeWithHistoryStopsTransactional(t *testing.T) {
	s.expectNewVersionNeeded(t, []string{addedType, renamedType})
}

func (s suite) allTypesWithHistoryStopsTransactional(t *testing.T) {
	s.expectNewVersionNeeded(t, nil)
}

// expectNewVersionNeeded widens a Transactional Consumer to types after an
// event of an added type was passed at or below its Cursor. The Consumer
// must go Stopped and stay Stopped across Resume, which can't fill the gap.
func (s suite) expectNewVersionNeeded(t *testing.T, types []string) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.write(ctx, renamed{Name: "missed"}, added{Name: "a"})
	var log typeLog
	h.deploy(release{consumer.Transactional, []string{addedType}}, &log).drain()

	widened := h.deploy(release{consumer.Transactional, types}, &log)
	if err := widened.c.Drain(context.Background()); !errors.Is(err, consumer.ErrStopped) {
		t.Fatalf("Drain after widening = %v, want ErrStopped", err)
	}
	st := widened.status()
	if st.Stopped == nil || !strings.Contains(st.Stopped.Cause, "new Projection version") {
		t.Fatalf("Status = %+v, want Stopped with a cause asking for a new Projection version", st)
	}
	if err := widened.rt.Resume(context.Background(), "filter"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := widened.c.Drain(context.Background()); !errors.Is(err, consumer.ErrStopped) {
		t.Errorf("Drain after Resume = %v, want ErrStopped again", err)
	}
	if got, want := log.get(), []string{addedType}; !slices.Equal(got, want) {
		t.Errorf("handled %v, want %v", got, want)
	}
}

func (s suite) addedTypeOnOwnTransactionAppliesFromCursor(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.write(ctx, renamed{Name: "history"}, added{Name: "a"})
	var log typeLog
	h.deploy(release{consumer.OwnTransaction, []string{addedType}}, &log).drain()

	widened := h.deploy(release{consumer.OwnTransaction, []string{addedType, renamedType}}, &log)
	widened.drain()
	h.write(ctx, renamed{Name: "new"})
	widened.drain()

	if got, want := log.get(), []string{addedType, renamedType}; !slices.Equal(got, want) {
		t.Errorf("handled %v, want %v (history below the Cursor not replayed)", got, want)
	}
	if st := widened.status(); st.Stopped != nil {
		t.Errorf("Status = %+v, want not Stopped", st)
	}
}

func (s suite) acceptedChangeNotRecheckedAfterRestart(t *testing.T) {
	h := s.setup(t)
	ctx := userCtx("t1")
	h.write(ctx, added{Name: "a"})
	var log typeLog
	h.deploy(release{consumer.Transactional, []string{addedType}}, &log).drain()

	widenedTypes := []string{addedType, renamedType}
	widened := h.deploy(release{consumer.Transactional, widenedTypes}, &log)
	widened.drain()
	h.write(ctx, renamed{Name: "b"})
	widened.drain()

	restarted := h.deploy(release{consumer.Transactional, widenedTypes}, &log)
	restarted.drain()
	if st := restarted.status(); st.Stopped != nil {
		t.Errorf("Status after restart = %+v, want not Stopped", st)
	}
}
