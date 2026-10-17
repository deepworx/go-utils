package scheduler_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
	"github.com/deepworx/go-utils/pkg/scheduler"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

func TestNewRejectsMissingLocation(t *testing.T) {
	t.Parallel()

	pool := pgtest.New(t)
	_, err := scheduler.New(pool, lease.NewClaims(pool, lease.Config{}), scheduler.Config{})
	if !errors.Is(err, scheduler.ErrNoLocation) {
		t.Fatalf("New() error = %v, want ErrNoLocation", err)
	}
}

func TestRegisterRejectsInvalidJobs(t *testing.T) {
	t.Parallel()

	noop := func(context.Context, scheduler.Run) (scheduler.Outcome, error) { return scheduler.Outcome{}, nil }
	tests := []struct {
		name    string
		jobs    []scheduler.Job
		wantErr error
	}{
		{
			name:    "duplicate name",
			jobs:    []scheduler.Job{{Name: "eod", Run: noop}, {Name: "eod", Run: noop}},
			wantErr: scheduler.ErrDuplicate,
		},
		{name: "empty name", jobs: []scheduler.Job{{Run: noop}}, wantErr: scheduler.ErrInvalidJob},
		{name: "missing Run", jobs: []scheduler.Job{{Name: "eod"}}, wantErr: scheduler.ErrInvalidJob},
		{
			name:    "NotBefore of a day or more",
			jobs:    []scheduler.Job{{Name: "eod", NotBefore: 24 * time.Hour, Run: noop}},
			wantErr: scheduler.ErrInvalidJob,
		},
		{
			name:    "interval job",
			jobs:    []scheduler.Job{{Name: "eod", Every: time.Minute, Run: noop}},
			wantErr: scheduler.ErrInvalidJob,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newScheduler(t, scheduler.Config{Location: berlin(t)})
			var err error
			for _, job := range tt.jobs {
				err = s.Register(job)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Register() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}

func newScheduler(t *testing.T, cfg scheduler.Config, opts ...scheduler.Option) *scheduler.Scheduler {
	t.Helper()
	pool := pgtest.New(t)
	s, err := scheduler.New(pool, lease.NewClaims(pool, lease.Config{}), cfg, opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return s
}
