// Package backoff computes jittered retry delays, fixed step schedules and
// poll intervals for the event-sourcing, scheduler and inbox runtimes.
//
// Jitter only ever shortens a delay, so a configured delay is always an
// upper bound.
package backoff

import (
	"math/rand/v2"
	"time"
)

// Source draws jitter. *rand.Rand from math/rand/v2 satisfies it; a nil
// Source uses the global math/rand/v2 generator.
type Source interface {
	// Int64N returns a value in [0, n).
	Int64N(n int64) int64
}

// Retry describes jittered exponential retry delays.
type Retry struct {
	// MaxAttempts is the total number of attempts, including the first.
	MaxAttempts int
	// Base is the delay after the first failed attempt.
	Base time.Duration
	// Max caps every delay.
	Max time.Duration
}

// Delay returns the wait after failed attempts and before the next one.
// The exponential delay min(Base*2^(failed-1), Max) is jittered down by up
// to half. It reports false once failed reaches MaxAttempts or when failed
// is below 1.
func (r Retry) Delay(failed int, src Source) (time.Duration, bool) {
	if failed < 1 || failed >= r.MaxAttempts {
		return 0, false
	}
	return jitter(r.exponential(failed), 2, src), true
}

func (r Retry) exponential(failed int) time.Duration {
	shift := failed - 1
	if shift >= 63 || r.Base > r.Max>>shift {
		return r.Max
	}
	return r.Base << shift
}

// Steps is a fixed delay schedule, such as Reactor auto-redrive or inbox
// retries.
type Steps []time.Duration

// Delay returns the step for the given number of failures, jittered down by
// up to a fifth. It reports false once the schedule is exhausted or when
// failed is below 1.
func (s Steps) Delay(failed int, src Source) (time.Duration, bool) {
	if failed < 1 || failed > len(s) {
		return 0, false
	}
	return jitter(s[failed-1], 5, src), true
}

// Poll returns interval jittered down by up to a fifth, so that pods polling
// the same table spread out.
func Poll(interval time.Duration, src Source) time.Duration {
	return jitter(interval, 5, src)
}

// jitter returns a value in [d - d/divisor, d].
func jitter(d time.Duration, divisor int64, src Source) time.Duration {
	span := int64(d) / divisor
	if span <= 0 {
		return d
	}
	return d - time.Duration(span) + time.Duration(draw(src, span+1))
}

func draw(src Source, n int64) int64 {
	if src == nil {
		return rand.Int64N(n)
	}
	return src.Int64N(n)
}
