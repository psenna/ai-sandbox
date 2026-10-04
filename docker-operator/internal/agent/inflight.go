package agent

import (
	"errors"
	"sync"
)

// ErrOperationInFlight is returned by Update and Delete when another
// operation for the same agent ID is already in flight IN THIS PROCESS --
// most likely a concurrent request, or the periodic reconcile pass working
// that same record. internal/api maps it to a 409.
var ErrOperationInFlight = errors.New("another operation is already in flight for this agent")

// IsOperationInFlight reports whether err was caused by an operation already
// being in flight for the same agent.
func IsOperationInFlight(err error) bool { return errors.Is(err, ErrOperationInFlight) }

// opRegistry tracks which agent IDs have an operation in flight IN THIS
// PROCESS: a Create, an in-place Update, a Delete, or the reconcile pass's
// own per-record teardown/wake.
//
// It exists so a PERIODIC reconcile pass can tell two states apart that the
// startup-only pass never had to. Reconcile's torn-down and marked-error
// branches are sound only against a record a CRASH left behind -- a record in
// StatusUpdating is proof of a crashed update, one in StatusCreating or
// StatusDeleting is proof of a crashed create/delete. On a ticker those same
// states are exactly what a HEALTHY in-flight create/update/delete looks like,
// so without this registry a naive ticker would corrupt healthy agents, rarely
// and confusingly.
//
// The insight that makes it safe: "is this operation in flight?" is a question
// about THIS PROCESS, and an in-memory registry has exactly the right lifetime.
// A record a crash left stuck has nothing in flight, because the process that
// would have been running its operation is gone -- which is precisely when the
// record stops being stuck. Nothing here is persisted, and the startup pass is
// unaffected: at startup nothing is in flight, so nothing is ever skipped.
//
// A refcount rather than a bool, because an ID can legitimately be held more
// than once at a time (the reconcile pass's wake path takes its own entry
// while a flow it drives may take one too), and a nested release must not free
// an entry an outer holder still needs. Every method is safe for concurrent
// use (guarded by mu) and the zero value is usable.
type opRegistry struct {
	mu   sync.Mutex
	refs map[string]int
}

// begin takes a reference for id, creating the entry if it is not already
// held. It never blocks and never fails: use it where the caller has
// exclusively created the ID (Create's freshly generated ID) or has already
// established via tryBegin that it owns the ID.
func (r *opRegistry) begin(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refs == nil {
		r.refs = make(map[string]int)
	}
	r.refs[id]++
}

// tryBegin takes a reference for id only if none is already held, reporting
// whether it succeeded. It is the atomic consult-and-reserve a flow uses to
// refuse to start (Update/Delete -> ErrOperationInFlight) or a reconcile pass
// uses to SKIP a record some other operation is already working -- atomic so
// that a concurrent begin cannot slip between a "busy?" check and the reserve.
func (r *opRegistry) tryBegin(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refs[id] > 0 {
		return false
	}
	if r.refs == nil {
		r.refs = make(map[string]int)
	}
	r.refs[id] = 1
	return true
}

// release drops one reference for id, deleting the entry once its count
// reaches zero so the map does not grow without bound over a long uptime. A
// release with no outstanding reference is a harmless no-op.
func (r *opRegistry) release(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch n := r.refs[id]; {
	case n <= 0:
		return
	case n == 1:
		delete(r.refs, id)
	default:
		r.refs[id] = n - 1
	}
}

// busy reports whether any reference for id is currently held. It does NOT
// reserve anything: use it for a read-only check (e.g. filtering a report)
// and tryBegin where the caller must also claim the ID.
func (r *opRegistry) busy(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refs[id] > 0
}
