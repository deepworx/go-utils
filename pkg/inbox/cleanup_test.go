package inbox_test

import (
	"testing"
	"time"

	"github.com/deepworx/go-utils/pkg/inbox"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
	"github.com/deepworx/go-utils/pkg/scheduler"
)

func TestCleanupJobDeletesSettledMessagesOlderThanRetention(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		retention time.Duration
		advance   time.Duration
		want      []string
	}{
		{
			name:    "default retention of 7 days",
			advance: 7 * 24 * time.Hour,
			want:    []string{"parked-old", "pending-old", "done-new"},
		},
		{
			name:    "settled exactly the default retention ago are kept",
			advance: 7*24*time.Hour - time.Hour,
			want:    []string{"done-old", "ignored-old", "parked-old", "discarded-old", "pending-old", "done-new"},
		},
		{
			name:      "custom retention",
			retention: time.Hour,
			advance:   time.Hour,
			want:      []string{"parked-old", "pending-old", "done-new"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clk := newClock()
			store, pool := newStore(t, inbox.WithClock(clk.Now))
			settleOneOfEach(t, store, clk)
			clk.Advance(tt.advance)

			claims := lease.NewClaims(pool, lease.Config{})
			startBackground(t, claims.Run)
			waitConnected(t, claims)
			s, err := scheduler.New(scheduler.Config{Pool: pool, Claims: claims, Location: time.UTC})
			if err != nil {
				t.Fatalf("scheduler.New() error = %v", err)
			}
			if err := s.Register(inbox.CleanupJob(store, tt.retention)); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			if err := s.Tick(t.Context(), clk.Now()); err != nil {
				t.Fatalf("Tick() error = %v", err)
			}

			assertRemaining(t, store, tt.want)
		})
	}
}
