package sandboxctl

import (
	"context"
	"math/rand"
	"time"

	"github.com/go-logr/logr"

	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// retryStep retries fn while storage.IsRetryable(err) -- i.e. only
// ErrUnreachable-kinded failures. ErrNotFound/ErrInvalid/ErrPermission/
// ErrCorrupt are permanent and surface immediately. Exponential backoff
// with jitter, bounded by retries; sleep is injected so tests run
// instantly.
//
// Each attempt runs under its own stepTimeout-bounded context, not the bare
// outer ctx: internal/storage's S3 client is built with a plain
// http.Client{} (no Timeout, see s3.go), so a single slow or wedged backend
// call would otherwise hang indefinitely -- defeating the whole point of a
// "bounded" retry budget, since the loop can never even reach its next
// attempt, let alone give up and report Failed. A context deadline hit
// mid-call is classified ErrUnreachable (retryable) by classifySnapshotErr/
// classifyRestoreErr/classifyS3Error, so a timed-out attempt is treated
// exactly like any other transient failure and counts toward retries.
//
// Shared by SnapshotHook (freeze) and RestoreHook (wake, #29) -- there is
// exactly ONE implementation of this backoff/jitter/per-attempt-timeout
// logic, never two copies that could silently drift.
func retryStep(ctx context.Context, log logr.Logger, sleep func(context.Context, time.Duration) error, retries int, stepTimeout time.Duration, seq int, step string, fn func(context.Context) error) error {
	delays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if retries <= 0 {
		retries = defaultSnapshotRetries
	}
	if stepTimeout <= 0 {
		stepTimeout = defaultSnapshotStepTimeout
	}
	if sleep == nil {
		sleep = defaultSleep
	}

	var err error
	for attempt := 0; ; attempt++ {
		stepCtx, cancel := context.WithTimeout(ctx, stepTimeout)
		err = fn(stepCtx)
		cancel()
		if err == nil {
			return nil
		}
		if !storage.IsRetryable(err) || attempt >= retries {
			return err
		}
		delay := delays[len(delays)-1]
		if attempt < len(delays) {
			delay = delays[attempt]
		}
		// jitter: +/- 20%
		jitter := time.Duration(rand.Int63n(int64(delay) / 5)) //nolint:gosec // G404: jitter timing, not security-sensitive
		delay = delay - delay/10 + jitter
		log.Info("retrying step", "seq", seq, "step", step, "attempt", attempt+1, "error", err.Error())
		if serr := sleep(ctx, delay); serr != nil {
			return serr
		}
	}
}

// defaultSleep is the real time.Sleep-backed implementation used when a
// hook's Sleep field is left nil.
func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
