package lease_test

import (
	"context"
	"testing"
	"time"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
)

// A ping that takes longer than the ping interval is slow, not lost: the
// session and its locks are still there, so held leases must survive it.
func TestLeaseSurvivesPingsSlowerThanInterval(t *testing.T) {
	t.Parallel()

	claims := lease.NewClaims(pgtest.New(t), lease.Config{PingInterval: time.Microsecond})
	startClaims(t, claims)
	ctx, _ := mustAcquire(t, claims, lease.Key{Namespace: "ns", Name: "slow_ping"})

	select {
	case <-ctx.Done():
		t.Fatalf("lease lost: %v", context.Cause(ctx))
	case <-time.After(500 * time.Millisecond):
	}
}
