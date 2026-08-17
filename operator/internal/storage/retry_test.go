package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeClock records every requested sleep duration without ever actually
// sleeping, so retry tests run instantly.
type fakeClock struct {
	sleeps []time.Duration
}

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.sleeps = append(c.sleeps, d)
	return nil
}

// fixedRand returns a deterministic sequence of values in [0,1); zero
// jitter (0.5, the midpoint) unless a test needs otherwise.
func fixedRand(v float64) func() float64 {
	return func() float64 { return v }
}

func TestRetryDo_SucceedsFirstTry(t *testing.T) {
	clock := &fakeClock{}
	calls := 0
	err := retryDo(context.Background(), RetryPolicy{}, clock.sleep, fixedRand(0.5), func(int) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("retryDo: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if len(clock.sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", clock.sleeps)
	}
}

func TestRetryDo_SucceedsOnThirdAttempt(t *testing.T) {
	clock := &fakeClock{}
	calls := 0
	err := retryDo(context.Background(), RetryPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: 10 * time.Second},
		clock.sleep, fixedRand(0.5), func(int) error {
			calls++
			if calls < 3 {
				return &Error{Kind: ErrUnreachable}
			}
			return nil
		})
	if err != nil {
		t.Fatalf("retryDo: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if len(clock.sleeps) != 2 {
		t.Fatalf("sleeps = %v, want 2 entries", clock.sleeps)
	}
	if clock.sleeps[1] <= clock.sleeps[0] {
		t.Errorf("delay did not grow: %v then %v", clock.sleeps[0], clock.sleeps[1])
	}
	for _, d := range clock.sleeps {
		if d > 10*time.Second {
			t.Errorf("sleep %v exceeds MaxDelay", d)
		}
	}
}

func TestRetryDo_ExhaustsBudgetReturnsLastError(t *testing.T) {
	clock := &fakeClock{}
	calls := 0
	sentinel := &Error{Kind: ErrUnreachable, Err: errors.New("boom")}
	err := retryDo(context.Background(), RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: time.Second},
		clock.sleep, fixedRand(0.5), func(int) error {
			calls++
			return sentinel
		})
	if calls != 4 {
		t.Errorf("calls = %d, want 4 (MaxAttempts)", calls)
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("returned error lost its Kind: %v", err)
	}
	if len(clock.sleeps) != 3 {
		t.Errorf("sleeps = %d, want MaxAttempts-1 = 3", len(clock.sleeps))
	}
}

func TestRetryDo_NotFoundReturnsAfterOneAttempt(t *testing.T) {
	clock := &fakeClock{}
	calls := 0
	err := retryDo(context.Background(), RetryPolicy{}, clock.sleep, fixedRand(0.5), func(int) error {
		calls++
		return &Error{Kind: ErrNotFound}
	})
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (NotFound must not consume a retry)", calls)
	}
	if !IsNotFound(err) {
		t.Errorf("err = %v, want NotFound-kinded", err)
	}
	if len(clock.sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", clock.sleeps)
	}
}

func TestRetryDo_InvalidPermissionCorruptReturnImmediately(t *testing.T) {
	for _, kind := range []error{ErrInvalid, ErrPermission, ErrCorrupt} {
		clock := &fakeClock{}
		calls := 0
		_ = retryDo(context.Background(), RetryPolicy{}, clock.sleep, fixedRand(0.5), func(int) error {
			calls++
			return &Error{Kind: kind}
		})
		if calls != 1 {
			t.Errorf("kind=%v: calls = %d, want 1", kind, calls)
		}
	}
}

func TestRetryDo_ContextCancelledReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	clock := &fakeClock{}
	err := retryDo(ctx, RetryPolicy{}, clock.sleep, fixedRand(0.5), func(int) error {
		calls++
		return &Error{Kind: ErrUnreachable}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("calls = %d, want 0 (ctx already cancelled before the first attempt)", calls)
	}
}

func TestRetryDo_SleepCancellationPropagates(t *testing.T) {
	ctx := context.Background()
	calls := 0
	sleep := func(context.Context, time.Duration) error { return context.Canceled }
	err := retryDo(ctx, RetryPolicy{MaxAttempts: 3}, sleep, fixedRand(0.5), func(int) error {
		calls++
		return &Error{Kind: ErrUnreachable}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (sleep before 2nd attempt failed)", calls)
	}
}

func TestJittered_StaysWithinBounds(t *testing.T) {
	d := 100 * time.Millisecond
	frac := 0.2
	for _, r := range []float64{0, 0.25, 0.5, 0.75, 0.999} {
		got := jittered(d, frac, fixedRand(r))
		lo := time.Duration(float64(d) * (1 - frac))
		hi := time.Duration(float64(d) * (1 + frac))
		if got < lo || got > hi {
			t.Errorf("jittered(%v, %v, rnd=%v) = %v, want within [%v, %v]", d, frac, r, got, lo, hi)
		}
	}
}

func TestDefaultRetryPolicy_Values(t *testing.T) {
	p := DefaultRetryPolicy()
	if p.MaxAttempts != 5 || p.BaseDelay != 100*time.Millisecond || p.MaxDelay != 5*time.Second || p.Jitter != 0.2 {
		t.Errorf("DefaultRetryPolicy() = %+v, want {5, 100ms, 5s, 0.2}", p)
	}
}

func TestRetryPolicy_WithDefaultsFillsZeroFieldsOnly(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 9}.withDefaults()
	if p.MaxAttempts != 9 {
		t.Errorf("MaxAttempts overridden: %d", p.MaxAttempts)
	}
	if p.BaseDelay != 100*time.Millisecond {
		t.Errorf("BaseDelay not defaulted: %v", p.BaseDelay)
	}
}
