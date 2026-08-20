package sandboxctl

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// sequenceStore is a Store double whose Snapshot() cycles through a fixed
// sequence of phases, one step per Refresh call.
type sequenceStore struct {
	mu  sync.Mutex
	seq []v1alpha1.Phase
	idx int
}

func (s *sequenceStore) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx == 0 {
		return Snapshot{}
	}
	return Snapshot{Phase: s.seq[s.idx-1], Fresh: true, ObservedAt: time.Now()}
}

func (s *sequenceStore) Refresh(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx < len(s.seq) {
		s.idx++
	}
	return nil
}

func (s *sequenceStore) DeclareWait(context.Context, WaitProbe, time.Time) error { return nil }
func (s *sequenceStore) ReportDone(context.Context, Result, time.Time) (bool, error) {
	return false, nil
}
func (s *sequenceStore) RecordSnapshotAttempt(context.Context, SnapshotAttempt, time.Time) error {
	return nil
}
func (s *sequenceStore) RecordSnapshot(context.Context, SnapshotRecord, time.Time) error {
	return nil
}
func (s *sequenceStore) RecordRestoreAttempt(context.Context, RestoreAttempt, time.Time) error {
	return nil
}
func (s *sequenceStore) PatchArchive(context.Context, ArchivePatch) error { return nil }

type countingHook struct {
	mu    sync.Mutex
	calls int
	last  Snapshot
}

func (h *countingHook) Freeze(_ context.Context, s Snapshot) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.last = s
	return nil
}

func (h *countingHook) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func TestPoller_LatchesOnceOnFreezingAndDoesNotUnlatch(t *testing.T) {
	store := &sequenceStore{seq: []v1alpha1.Phase{
		v1alpha1.PhaseRunning,
		v1alpha1.PhaseFreezing,
		v1alpha1.PhaseFreezing,
		v1alpha1.PhaseRunning, // flip back -- must NOT un-latch
	}}
	hook := &countingHook{}
	p := NewPoller(store, time.Second, hook, nil)
	ctx := context.Background()

	if p.Freezing() {
		t.Fatal("Freezing() = true before any poll")
	}

	p.tick(ctx) // Running
	if p.Freezing() {
		t.Fatal("Freezing() = true after observing Running")
	}

	p.tick(ctx) // Freezing (first observation)
	if !p.Freezing() {
		t.Fatal("Freezing() = false after observing Freezing")
	}
	if got := hook.callCount(); got != 1 {
		t.Fatalf("hook called %d times, want 1", got)
	}

	p.tick(ctx) // still Freezing
	if got := hook.callCount(); got != 1 {
		t.Fatalf("hook called %d times after a second Freezing observation, want still 1", got)
	}

	p.tick(ctx) // flip back to Running
	if !p.Freezing() {
		t.Error("Freezing() = false after flip-back to Running, want it to stay latched")
	}
	if got := hook.callCount(); got != 1 {
		t.Errorf("hook called %d times after flip-back, want still 1", got)
	}
}

func TestPoller_LatchFreezing_Forces(t *testing.T) {
	store := &sequenceStore{seq: []v1alpha1.Phase{v1alpha1.PhaseRunning}}
	hook := &countingHook{}
	p := NewPoller(store, time.Second, hook, nil)

	p.LatchFreezing(context.Background())
	if !p.Freezing() {
		t.Fatal("Freezing() = false after LatchFreezing")
	}
	if got := hook.callCount(); got != 1 {
		t.Fatalf("hook called %d times, want 1", got)
	}

	p.LatchFreezing(context.Background())
	if got := hook.callCount(); got != 1 {
		t.Fatalf("hook called %d times after a second LatchFreezing, want still 1", got)
	}
}

func TestPoller_ProgressRingBufferCapsAt32(t *testing.T) {
	store := &sequenceStore{}
	p := NewPoller(store, time.Second, nil, nil)

	for i := 0; i < 40; i++ {
		p.AddProgress(string(rune('a' + i%26)))
	}
	got := p.Progress()
	if len(got) != progressRingCap {
		t.Fatalf("len(Progress()) = %d, want %d", len(got), progressRingCap)
	}
}

func TestPoller_Stale(t *testing.T) {
	store := &sequenceStore{}
	p := NewPoller(store, time.Second, nil, nil)

	fresh := Snapshot{Fresh: true, ObservedAt: time.Now()}
	if p.Stale(fresh, time.Now()) {
		t.Error("Stale() = true for a just-observed snapshot")
	}

	old := Snapshot{Fresh: true, ObservedAt: time.Now().Add(-10 * time.Second)}
	if !p.Stale(old, time.Now()) {
		t.Error("Stale() = false for a snapshot older than 3x the poll interval")
	}

	neverObserved := Snapshot{Fresh: false}
	if !p.Stale(neverObserved, time.Now()) {
		t.Error("Stale() = false for a never-successfully-observed snapshot")
	}
}
