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

func TestNewRejectsIncompleteConfig(t *testing.T) {
	t.Parallel()

	pool := pgtest.New(t)
	claims := lease.NewClaims(pool, lease.Config{})
	tests := []struct {
		name    string
		cfg     scheduler.Config
		wantErr error
	}{
		{name: "no location", cfg: scheduler.Config{Pool: pool, Claims: claims}, wantErr: scheduler.ErrNoLocation},
		{name: "no pool", cfg: scheduler.Config{Claims: claims, Location: time.UTC}, wantErr: scheduler.ErrInvalidConfig},
		{name: "no claims", cfg: scheduler.Config{Pool: pool, Location: time.UTC}, wantErr: scheduler.ErrInvalidConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := scheduler.New(tt.cfg); !errors.Is(err, tt.wantErr) {
				t.Errorf("New() error = %v, want %v", err, tt.wantErr)
			}
		})
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
			name:    "negative Every",
			jobs:    []scheduler.Job{{Name: "sweep", Every: -time.Minute, Run: noop}},
			wantErr: scheduler.ErrInvalidJob,
		},
		{
			name:    "NotBefore on an interval job",
			jobs:    []scheduler.Job{{Name: "sweep", Every: time.Minute, NotBefore: time.Hour, Run: noop}},
			wantErr: scheduler.ErrInvalidJob,
		},
		{
			name:    "duplicate name across daily and interval jobs",
			jobs:    []scheduler.Job{{Name: "eod", Run: noop}, {Name: "eod", Every: time.Minute, Run: noop}},
			wantErr: scheduler.ErrDuplicate,
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
	cfg.Pool, cfg.Claims = pool, lease.NewClaims(pool, lease.Config{})
	s, err := scheduler.New(cfg, opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return s
}
