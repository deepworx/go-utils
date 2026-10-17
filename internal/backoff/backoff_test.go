package backoff_test

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/deepworx/go-utils/internal/backoff"
)

// maxSource always draws the largest value, so jitter removes nothing.
type maxSource struct{}

func (maxSource) Int64N(n int64) int64 { return n - 1 }

// zeroSource always draws zero, so jitter removes the most it can.
type zeroSource struct{}

func (zeroSource) Int64N(int64) int64 { return 0 }

func TestRetryDelay(t *testing.T) {
	t.Parallel()

	retry := backoff.Retry{MaxAttempts: 5, Base: time.Second, Max: 5 * time.Second}

	tests := []struct {
		name   string
		src    backoff.Source
		failed int
		want   time.Duration
		wantOK bool
	}{
		{name: "first failure waits base", src: maxSource{}, failed: 1, want: time.Second, wantOK: true},
		{name: "second failure doubles", src: maxSource{}, failed: 2, want: 2 * time.Second, wantOK: true},
		{name: "third failure doubles again", src: maxSource{}, failed: 3, want: 4 * time.Second, wantOK: true},
		{name: "fourth failure capped at max", src: maxSource{}, failed: 4, want: 5 * time.Second, wantOK: true},
		{name: "jitter at most halves first delay", src: zeroSource{}, failed: 1, want: 500 * time.Millisecond, wantOK: true},
		{name: "jitter at most halves capped delay", src: zeroSource{}, failed: 4, want: 2500 * time.Millisecond, wantOK: true},
		{name: "attempts exhausted", src: maxSource{}, failed: 5, wantOK: false},
		{name: "zero failures invalid", src: maxSource{}, failed: 0, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := retry.Delay(tt.failed, tt.src)
			if ok != tt.wantOK {
				t.Fatalf("Delay(%d) ok = %v, want %v", tt.failed, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("Delay(%d) = %v, want %v", tt.failed, got, tt.want)
			}
		})
	}
}

func TestRetryDelayStaysWithinBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		retry backoff.Retry
	}{
		{name: "small base", retry: backoff.Retry{MaxAttempts: 10, Base: 10 * time.Millisecond, Max: time.Second}},
		{name: "base above max", retry: backoff.Retry{MaxAttempts: 10, Base: time.Minute, Max: time.Second}},
		{name: "many attempts do not overflow", retry: backoff.Retry{MaxAttempts: 200, Base: time.Second, Max: time.Hour}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := rand.New(rand.NewPCG(1, 2))
			for failed := 1; failed < tt.retry.MaxAttempts; failed++ {
				got, ok := tt.retry.Delay(failed, src)
				if !ok {
					t.Fatalf("Delay(%d) not ok", failed)
				}
				lower := min(tt.retry.Base, tt.retry.Max) / 2
				if got < lower || got > tt.retry.Max {
					t.Fatalf("Delay(%d) = %v, out of bounds for %+v", failed, got, tt.retry)
				}
			}
		})
	}
}

func TestDelaysDeterministicUnderSeededSource(t *testing.T) {
	t.Parallel()

	retry := backoff.Retry{MaxAttempts: 8, Base: 100 * time.Millisecond, Max: 10 * time.Second}
	steps := backoff.Steps{time.Second, time.Minute, time.Hour}

	tests := []struct {
		name  string
		delay func(n int, src backoff.Source) (time.Duration, bool)
		count int
	}{
		{name: "retry", delay: retry.Delay, count: retry.MaxAttempts - 1},
		{name: "steps", delay: steps.Delay, count: len(steps)},
		{name: "poll", delay: func(_ int, src backoff.Source) (time.Duration, bool) {
			return backoff.Poll(time.Second, src), true
		}, count: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			first := rand.New(rand.NewPCG(42, 7))
			second := rand.New(rand.NewPCG(42, 7))
			for n := 1; n <= tt.count; n++ {
				a, _ := tt.delay(n, first)
				b, _ := tt.delay(n, second)
				if a != b {
					t.Fatalf("delay(%d) differs under identical seeds: %v vs %v", n, a, b)
				}
			}
		})
	}
}

func TestStepsDelay(t *testing.T) {
	t.Parallel()

	steps := backoff.Steps{time.Minute, 5 * time.Minute, 30 * time.Minute}

	tests := []struct {
		name   string
		src    backoff.Source
		failed int
		want   time.Duration
		wantOK bool
	}{
		{name: "first step without jitter", src: maxSource{}, failed: 1, want: time.Minute, wantOK: true},
		{name: "last step without jitter", src: maxSource{}, failed: 3, want: 30 * time.Minute, wantOK: true},
		{name: "jitter removes at most a fifth", src: zeroSource{}, failed: 2, want: 4 * time.Minute, wantOK: true},
		{name: "schedule exhausted", src: maxSource{}, failed: 4, wantOK: false},
		{name: "zero failures invalid", src: maxSource{}, failed: 0, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := steps.Delay(tt.failed, tt.src)
			if ok != tt.wantOK {
				t.Fatalf("Delay(%d) ok = %v, want %v", tt.failed, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("Delay(%d) = %v, want %v", tt.failed, got, tt.want)
			}
		})
	}
}

func TestStepsDelayEmptyScheduleIsExhausted(t *testing.T) {
	t.Parallel()

	if _, ok := backoff.Steps(nil).Delay(1, maxSource{}); ok {
		t.Fatal("empty Steps returned a delay")
	}
}

func TestPoll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		interval time.Duration
		src      backoff.Source
		want     time.Duration
	}{
		{name: "upper bound is the interval", interval: time.Second, src: maxSource{}, want: time.Second},
		{name: "jitter removes at most a fifth", interval: time.Second, src: zeroSource{}, want: 800 * time.Millisecond},
		{name: "zero interval stays zero", interval: 0, src: maxSource{}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := backoff.Poll(tt.interval, tt.src); got != tt.want {
				t.Errorf("Poll(%v) = %v, want %v", tt.interval, got, tt.want)
			}
		})
	}
}

func TestNilSourceUsesGlobalRandomness(t *testing.T) {
	t.Parallel()

	for range 100 {
		got := backoff.Poll(time.Second, nil)
		if got < 800*time.Millisecond || got > time.Second {
			t.Fatalf("Poll(1s, nil) = %v, want in [800ms, 1s]", got)
		}
	}
}
