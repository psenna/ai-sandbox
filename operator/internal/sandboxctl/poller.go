package sandboxctl

import (
	"context"
	"sync"
	"time"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

const progressRingCap = 32

// Poller periodically GETs the sidecar's OWN SandboxEnvironment (via
// Store.Refresh) and caches the result. It polls rather than watching
// because the sidecar Role grants neither list nor watch -- an informer
// would need both and would fail closed at startup. One GET per interval
// against a single named object is negligible API load.
//
// Poller also owns the freeze latch and the in-memory progress ring buffer:
// both are naturally poll-cadence-scoped state, not per-request state.
type Poller struct {
	store    Store
	interval time.Duration
	hook     FreezeHook
	log      func(format string, args ...any)

	mu           sync.Mutex
	freezing     bool
	progressRing []string
}

// NewPoller builds a Poller. hook may be nil (treated as a no-op).
func NewPoller(store Store, interval time.Duration, hook FreezeHook, log func(format string, args ...any)) *Poller {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Poller{store: store, interval: interval, hook: hook, log: log}
}

// Run polls until ctx is cancelled. Performs one immediate poll before
// entering the ticker loop, so /v1/status has a fresh snapshot as soon as
// possible after startup rather than waiting a full interval.
func (p *Poller) Run(ctx context.Context) {
	p.tick(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

func (p *Poller) tick(ctx context.Context) {
	if err := p.store.Refresh(ctx); err != nil {
		// Success updates the cached snapshot + observedAt; failure logs at
		// V(1) equivalent and leaves the snapshot in place -- staleness on
		// /v1/status, not a hard error, is how this surfaces.
		p.log("poll failed: %v", err)
		return
	}
	snap := p.store.Snapshot()
	if snap.Phase == v1alpha1.PhaseFreezing {
		p.latchFreezing(ctx, snap)
	}
}

// latchFreezing sets freezing exactly once (never un-latches -- a later
// flip-back to a non-Freezing phase, which should not normally happen, does
// not un-freeze the control API) and invokes the FreezeHook exactly once.
//
// The hook runs INLINE on this goroutine (the poll loop, or run.go's
// SIGTERM path), so a real snapshot (#28's SnapshotHook) blocks the next
// poll tick and /v1/status goes stale for the whole snapshot duration. This
// is acceptable: the control API is already quiesced (every mutating
// endpoint answers 409 CodeFreezing) once Freezing() is true, so nothing is
// lost by /v1/status also going stale for the same window.
func (p *Poller) latchFreezing(ctx context.Context, snap Snapshot) {
	p.mu.Lock()
	already := p.freezing
	p.freezing = true
	p.mu.Unlock()
	if already {
		return
	}
	p.log("environment %s entered Freezing; quiescing control API", snap.Environment.Name)
	if p.hook != nil {
		if err := p.hook.Freeze(ctx, snap); err != nil {
			p.log("freeze hook failed: %v", err)
		}
	}
}

// LatchFreezing forces the freeze latch immediately (e.g. on SIGTERM, see
// run.go) without waiting for the next poll tick, using the last cached
// snapshot.
func (p *Poller) LatchFreezing(ctx context.Context) {
	p.latchFreezing(ctx, p.store.Snapshot())
}

// Freezing reports whether the freeze latch has fired.
func (p *Poller) Freezing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.freezing
}

// AddProgress appends msg to the ring buffer, keeping at most the most
// recent progressRingCap entries.
func (p *Poller) AddProgress(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.progressRing = append(p.progressRing, msg)
	if len(p.progressRing) > progressRingCap {
		p.progressRing = p.progressRing[len(p.progressRing)-progressRingCap:]
	}
}

// Progress returns a copy of the current ring buffer contents, oldest first.
func (p *Poller) Progress() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.progressRing))
	copy(out, p.progressRing)
	return out
}

// Stale reports whether snap's observation is older than 3x the poll
// interval, or was never successfully observed at all.
func (p *Poller) Stale(snap Snapshot, now time.Time) bool {
	if !snap.Fresh {
		return true
	}
	return now.Sub(snap.ObservedAt) > 3*p.interval
}
