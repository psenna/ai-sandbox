package storage

import (
	"context"
	"time"
)

// RetryPolicy configures retryDo's exponential backoff.
type RetryPolicy struct {
	// MaxAttempts is the maximum number of attempts (including the first),
	// default 5.
	MaxAttempts int
	// BaseDelay is the delay before the first retry, default 100ms.
	BaseDelay time.Duration
	// MaxDelay caps the backoff delay, default 5s.
	MaxDelay time.Duration
	// Jitter is the fractional jitter applied to each delay (0.2 means the
	// actual delay is uniformly drawn from [d*0.8, d*1.2]), default 0.2.
	Jitter float64
}

// DefaultRetryPolicy returns the package default: 5 attempts, 100ms base
// delay, 5s max delay, 0.2 jitter.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: 5 * time.Second, Jitter: 0.2}
}

// withDefaults fills any zero-valued field with DefaultRetryPolicy's value.
func (p RetryPolicy) withDefaults() RetryPolicy {
	d := DefaultRetryPolicy()
	if p.MaxAttempts > 0 {
		d.MaxAttempts = p.MaxAttempts
	}
	if p.BaseDelay > 0 {
		d.BaseDelay = p.BaseDelay
	}
	if p.MaxDelay > 0 {
		d.MaxDelay = p.MaxDelay
	}
	if p.Jitter > 0 {
		d.Jitter = p.Jitter
	}
	return d
}

// retryDo retries op with exponential backoff, up to p.MaxAttempts times.
// sleep and rnd are injected (never time.Sleep/math/rand directly) so tests
// run instantly with no wall clock -- this repo's established "time is
// always passed in" convention (see internal/lifecycle/doc.go).
//
// Scoping: retryDo wraps Get/Stat/List/Delete/DeletePrefix ONLY. It must
// NEVER wrap Put -- see backend.go and doc.go for why (the io.Reader body
// is consumed on the first attempt and is not rewindable).
//
// Only IsRetryable(err) errors consume a retry; anything else (NotFound,
// Invalid, Permission, Corrupt) returns immediately on the first attempt
// without sleeping.
func retryDo(
	ctx context.Context,
	p RetryPolicy,
	sleep func(context.Context, time.Duration) error,
	rnd func() float64,
	op func(attempt int) error,
) error {
	p = p.withDefaults()

	var lastErr error
	delay := p.BaseDelay
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		lastErr = op(attempt)
		if lastErr == nil {
			return nil
		}
		if !IsRetryable(lastErr) {
			return lastErr
		}
		if attempt == p.MaxAttempts {
			break
		}

		d := jittered(delay, p.Jitter, rnd)
		if d > p.MaxDelay {
			d = p.MaxDelay
		}
		if err := sleep(ctx, d); err != nil {
			return err
		}

		delay *= 2
		if delay > p.MaxDelay {
			delay = p.MaxDelay
		}
	}
	return lastErr
}

// jittered applies +/-frac jitter to d using rnd() (expected to return a
// value uniformly distributed in [0, 1)), then caps the result at max.
func jittered(d time.Duration, frac float64, rnd func() float64) time.Duration {
	if frac <= 0 {
		return d
	}
	// r in [-frac, frac]
	r := (rnd()*2 - 1) * frac
	out := time.Duration(float64(d) * (1 + r))
	if out < 0 {
		out = 0
	}
	return out
}
