package inbox

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Status is the state of a stored message.
type Status string

// Statuses of a stored message.
const (
	// StatusPending is due for a first attempt or a retry, or claimed.
	StatusPending Status = "pending"
	// StatusDone was handled.
	StatusDone Status = "done"
	// StatusIgnored was ignored by its Handler.
	StatusIgnored Status = "ignored"
	// StatusParked failed for good and waits for Redrive or Discard.
	StatusParked Status = "parked"
	// StatusDiscarded was discarded by an operator.
	StatusDiscarded Status = "discarded"
)

var statuses = []Status{StatusPending, StatusDone, StatusIgnored, StatusParked, StatusDiscarded}

// claimed is a message claimed by a Worker. generation fences its settle.
type claimed struct {
	Delivery
	generation   int64
	traceContext string
}

type claimInput struct {
	Source string
	Limit  int
	// Lease is how long the claim keeps the message from other Workers.
	Lease time.Duration
}

// claim takes up to in.Limit due pending messages of in.Source. Each claim
// counts as an attempt, bumps the claim generation and moves the message's
// due time to the end of the lease, so a message whose Worker died becomes
// due again once the lease has expired.
func (s *Store) claim(ctx context.Context, in claimInput) ([]claimed, error) {
	now := s.now()
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM `+s.table+`
			WHERE source = $1 AND status = 'pending' AND next_attempt_at <= $2
			ORDER BY next_attempt_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE `+s.table+` m
		SET attempts = m.attempts + 1,
		    claim_generation = m.claim_generation + 1,
		    claimed_until = $4,
		    next_attempt_at = $4
		FROM due
		WHERE m.id = due.id
		RETURNING m.id, m.key, m.type, m.tenant_id, m.payload,
		          m.attempts, m.claim_generation, m.received_at, COALESCE(m.trace_context, '')`,
		in.Source, now, in.Limit, now.Add(in.Lease),
	)
	if err != nil {
		return nil, fmt.Errorf("inbox: claim %s: %w", in.Source, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (claimed, error) {
		c := claimed{Delivery: Delivery{Message: Message{Source: in.Source}}}
		err := row.Scan(&c.ID, &c.Key, &c.Type, &c.Tenant, &c.Payload,
			&c.Attempt, &c.generation, &c.ReceivedAt, &c.traceContext)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("inbox: claim %s: %w", in.Source, err)
	}
	return out, nil
}

// expiredClaimError is the error stored on a message Parked because the
// claim of its last attempt expired without a settle.
const expiredClaimError = "inbox: last attempt never settled (claim expired)"

type parkExpiredInput struct {
	Source string
	// MaxAttempts is the number of attempts a Worker makes before it Parks.
	MaxAttempts int
}

// parkExpired Parks the due pending messages of in.Source whose last
// attempt was claimed but never settled, as when the handler took the
// process down, so they are not retried forever. It returns how many it
// Parked.
func (s *Store) parkExpired(ctx context.Context, in parkExpiredInput) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE `+s.table+`
		SET status = 'parked', error = $4, claimed_until = NULL
		WHERE id IN (
			SELECT id FROM `+s.table+`
			WHERE source = $1 AND status = 'pending' AND next_attempt_at <= $2 AND attempts >= $3
			FOR UPDATE SKIP LOCKED
		)`,
		in.Source, s.now(), in.MaxAttempts, expiredClaimError,
	)
	if err != nil {
		return 0, fmt.Errorf("inbox: park expired claims of %s: %w", in.Source, err)
	}
	return tag.RowsAffected(), nil
}

// release hands a claimed message back without counting the attempt, for
// a Worker that stops before its handler finished. It is fenced like
// settle and reports whether the row was updated.
func (s *Store) release(ctx context.Context, c claimed) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE `+s.table+`
		SET attempts = GREATEST(attempts - 1, 0), next_attempt_at = $3, claimed_until = NULL
		WHERE id = $1 AND claim_generation = $2 AND status = 'pending'`,
		c.ID, c.generation, s.now(),
	)
	if err != nil {
		return false, fmt.Errorf("inbox: release message %d: %w", c.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// settlement is the outcome of one claimed attempt.
type settlement struct {
	ID         int64
	Generation int64
	Status     Status
	Error      string
	// RetryAt is the next due time when Status is pending.
	RetryAt time.Time
}

// settle records st unless the claim generation moved on, which means the
// claim was lost to another Worker. It reports whether the row was updated.
func (s *Store) settle(ctx context.Context, st settlement) (bool, error) {
	now := s.now()
	var settledAt, retryAt *time.Time
	if st.Status == StatusPending {
		retryAt = &st.RetryAt
	} else {
		settledAt = &now
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE `+s.table+`
		SET status = $3,
		    error = NULLIF($4, ''),
		    settled_at = $5,
		    next_attempt_at = COALESCE($6, next_attempt_at),
		    claimed_until = NULL
		WHERE id = $1 AND claim_generation = $2 AND status = 'pending'`,
		st.ID, st.Generation, st.Status, st.Error, settledAt, retryAt,
	)
	if err != nil {
		return false, fmt.Errorf("inbox: settle message %d: %w", st.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// sourceCounts are the polled gauge values of one Source.
type sourceCounts struct {
	ByStatus map[Status]int64
	// OldestPending is the age of the oldest pending message, or 0.
	OldestPending time.Duration
}

func (s *Store) counts(ctx context.Context, source string) (sourceCounts, error) {
	out := sourceCounts{ByStatus: make(map[Status]int64, len(statuses))}
	for _, st := range statuses {
		out.ByStatus[st] = 0
	}
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM `+s.table+` WHERE source = $1 GROUP BY status`, source)
	if err != nil {
		return out, fmt.Errorf("inbox: count %s: %w", source, err)
	}
	var (
		status Status
		n      int64
	)
	_, err = pgx.ForEachRow(rows, []any{&status, &n}, func() error {
		out.ByStatus[status] = n
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("inbox: count %s: %w", source, err)
	}

	var oldest *time.Time
	err = s.pool.QueryRow(ctx, `SELECT min(received_at) FROM `+s.table+` WHERE source = $1 AND status = 'pending'`, source).Scan(&oldest)
	if err != nil {
		return out, fmt.Errorf("inbox: oldest pending %s: %w", source, err)
	}
	if oldest != nil {
		out.OldestPending = max(s.now().Sub(*oldest), 0)
	}
	return out, nil
}
