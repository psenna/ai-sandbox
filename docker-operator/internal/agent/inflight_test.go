package agent

import (
	"sync"
	"testing"
)

func TestOpRegistry_BeginReleaseBusy(t *testing.T) {
	var r opRegistry

	if r.busy("agt_1") {
		t.Fatal("busy on a fresh registry = true, want false")
	}
	r.begin("agt_1")
	if !r.busy("agt_1") {
		t.Fatal("busy after begin = false, want true")
	}
	if r.busy("agt_2") {
		t.Error("busy for an unrelated id = true, want false (entries are per-id)")
	}
	r.release("agt_1")
	if r.busy("agt_1") {
		t.Error("busy after release = true, want false")
	}
}

func TestOpRegistry_TryBegin(t *testing.T) {
	var r opRegistry

	if !r.tryBegin("agt_1") {
		t.Fatal("tryBegin on a free id = false, want true")
	}
	if r.tryBegin("agt_1") {
		t.Fatal("tryBegin on a held id = true, want false")
	}
	r.release("agt_1")
	if !r.tryBegin("agt_1") {
		t.Error("tryBegin after release = false, want true")
	}
}

// TestOpRegistry_Refcount pins the reason this is a refcount and not a bool: a
// nested hold must not be freed by an inner release.
func TestOpRegistry_Refcount(t *testing.T) {
	var r opRegistry

	r.begin("agt_1")
	r.begin("agt_1") // nested/second holder
	r.release("agt_1")
	if !r.busy("agt_1") {
		t.Fatal("busy after an inner release = false; want true (an outer reference is still held)")
	}
	r.release("agt_1")
	if r.busy("agt_1") {
		t.Error("busy after the final release = true, want false (the entry must be deleted at zero)")
	}
}

// TestOpRegistry_ReleaseAtZeroIsNoop proves an unmatched release (a retried or
// doubled release) neither panics nor resurrects an entry.
func TestOpRegistry_ReleaseAtZeroIsNoop(t *testing.T) {
	var r opRegistry
	r.release("never-held")
	r.release("never-held")
	if r.busy("never-held") {
		t.Error("busy after releasing an id that was never held = true, want false")
	}
}

// TestOpRegistry_TryBeginAfterBeginIsRefused proves begin and tryBegin share
// the same per-id accounting: an ID held via begin (the Create/wake path) is
// seen as busy by tryBegin (the Update/Delete path), which is exactly what
// makes the "another operation is already in flight" refusal work.
func TestOpRegistry_TryBeginAfterBeginIsRefused(t *testing.T) {
	var r opRegistry
	r.begin("agt_1")
	if r.tryBegin("agt_1") {
		t.Error("tryBegin on an id held via begin = true, want false")
	}
	r.release("agt_1")
	if !r.tryBegin("agt_1") {
		t.Error("tryBegin after the begin reference was released = false, want true")
	}
}

// TestOpRegistry_ConcurrentSafety hammers the registry from many goroutines so
// `go test -race` proves the mutex actually guards every method.
func TestOpRegistry_ConcurrentSafety(t *testing.T) {
	var r opRegistry
	const workers = 32

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.begin("shared")
				_ = r.busy("shared")
				r.release("shared")
				if r.tryBegin("shared") {
					r.release("shared")
				}
			}
		}()
	}
	wg.Wait()

	// Every begin was matched by exactly one release, so the entry must be
	// gone regardless of interleaving.
	if r.busy("shared") {
		t.Error("busy after every reference was released = true, want false")
	}
}
