package eventstoretest

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/scheduler"
)

// clock is a settable test clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// recordedCommand is a deposit that ran under its Command ID.
type recordedCommand struct {
	id  accountID
	cmd eventstore.CommandOption
}

// recordDeposit runs a deposit of 1 under a new Command ID.
func (h harness) recordDeposit(t *testing.T) recordedCommand {
	t.Helper()
	ctx := userCtx("t1")
	c := recordedCommand{id: h.openAccount(t, ctx), cmd: eventstore.WithCommandID(newCommandID())}
	if _, err := h.accounts.Update(ctx, c.id, deposit(1), c.cmd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	return c
}

// wantRecorded repeats c and checks whether it was replayed from its record
// or ran again as a new command.
func (h harness) wantRecorded(t *testing.T, c recordedCommand, recorded bool) {
	t.Helper()
	var runs atomic.Int32
	res, err := h.accounts.Update(userCtx("t1"), c.id, counted(&runs, deposit(1)), c.cmd)
	if err != nil {
		t.Fatalf("repeated Update: %v", err)
	}
	if res.Replayed != recorded || (runs.Load() == 0) != recorded {
		t.Fatalf("repeat replayed %v and ran fn %d times, want recorded %v", res.Replayed, runs.Load(), recorded)
	}
	want := accountState{version: 3, balance: 2}
	if recorded {
		want = accountState{version: 2, balance: 1}
	}
	h.wantAccount(t, c.id, want)
}

func (s suite) cleanupCommands(t *testing.T) {
	tests := []struct {
		name        string
		before      time.Duration
		wantDeleted int64
		wantOld     bool
		wantNew     bool
	}{
		{name: "recorded exactly at the cutoff are kept", before: 0, wantDeleted: 0, wantOld: true, wantNew: true},
		{name: "older than the cutoff are deleted", before: time.Hour, wantDeleted: 1, wantOld: false, wantNew: true},
		{name: "all before a later cutoff are deleted", before: 3 * time.Hour, wantDeleted: 2, wantOld: false, wantNew: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clk := &clock{now: epoch}
			h := s.setupWithClock(t, clk.Now)
			old := h.recordDeposit(t)
			clk.Advance(2 * time.Hour)
			recent := h.recordDeposit(t)

			deleted, err := h.store.CleanupCommands(t.Context(), epoch.Add(tt.before))
			if err != nil {
				t.Fatalf("CleanupCommands: %v", err)
			}
			if deleted != tt.wantDeleted {
				t.Fatalf("CleanupCommands deleted %d, want %d", deleted, tt.wantDeleted)
			}
			h.wantRecorded(t, old, tt.wantOld)
			h.wantRecorded(t, recent, tt.wantNew)
		})
	}
}

func (s suite) commandCleanupJob(t *testing.T) {
	tests := []struct {
		name           string
		retention      time.Duration
		advance        time.Duration
		wantCandidates int
	}{
		{name: "default retention", advance: eventstore.DefaultCommandRetention + time.Second, wantCandidates: 1},
		{name: "negative retention uses the default", retention: -time.Hour, advance: eventstore.DefaultCommandRetention + time.Second, wantCandidates: 1},
		{name: "recorded exactly the default retention ago are kept", advance: eventstore.DefaultCommandRetention, wantCandidates: 0},
		{name: "custom retention", retention: time.Hour, advance: time.Hour + time.Second, wantCandidates: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clk := &clock{now: epoch}
			h := s.setupWithClock(t, clk.Now)
			c := h.recordDeposit(t)
			clk.Advance(tt.advance)

			job := eventstore.CommandCleanupJob(h.store, tt.retention)
			if job.Name != eventstore.CommandCleanupJobName || job.Every != 0 {
				t.Fatalf("job = %q every %v, want daily %q", job.Name, job.Every, eventstore.CommandCleanupJobName)
			}
			out, err := job.Run(userCtx("t1"), scheduler.Run{})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if out.Candidates != tt.wantCandidates {
				t.Fatalf("Candidates = %d, want %d", out.Candidates, tt.wantCandidates)
			}
			h.wantRecorded(t, c, tt.wantCandidates == 0)
		})
	}
}
