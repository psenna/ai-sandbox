# Manual Agent Stop/Resume Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a person stop a running agent's containers on demand (without
destroying anything) and resume it later, picking its session back up.

**Architecture:** Two new `Manager` methods, `StopAgent`/`ResumeAgent`, built
on plumbing that already exists: the `StatusStopped` status, the
`Manager.inFlight` concurrency registry Update/Delete already share, and
`wakeAgent`'s existing session-preserving container-rebuild sequence
(generalized into a function `StopAgent`'s sibling `ResumeAgent` can share,
not reimplement). Two new HTTP routes expose them; two new items in the
agent detail page's existing options menu expose those.

**Tech Stack:** Go (`net/http`, bbolt via `internal/store`), the project's
own `dockerclient` abstraction and its `dockerclienttest.Fake`, vanilla JS
(`web/*.js`, `node --test`).

**Spec:** `docs/superpowers/specs/2026-10-13-agent-stop-resume-design.md`

## Global Constraints

- Resume is never a plain `ContainerStart` on the agent container — always
  stop+remove+recreate with `firstInvocationArgs(a, sessionResume)`, exactly
  like `wakeAgent` already does (spec decision 7).
- Stop stops BOTH the agent container and its DinD sidecar; so does the
  updated `ApplyOnComplete`'s `"stop"` case (spec decision 2).
- `ResumeAgent` applies uniformly to any `StatusStopped` agent regardless of
  how it got there — no "reason" field (spec decision 3).
- Concurrency goes through the existing `Manager.inFlight` registry — no new
  lock (spec decision 8).
- The UI surface is the agent detail page's existing `⋮` options menu only —
  never the sidebar/list (spec decision 6).
- Stop requires a client-side confirmation (non-danger styling); Resume does
  not (spec decisions 4-5).
- `wakeAgent`'s hot path — nothing needs waking, so zero store writes and
  zero rebuild calls — must not regress across the refactor that shares code
  with it (spec's explicit note on the extraction's boundary).
- Any edit under `web/` requires `make sync-web-embed` (or the manual
  equivalent: `rm -rf internal/webui/web && mkdir -p internal/webui/web &&
  cp -a web/. internal/webui/web/ && find internal/webui/web -name
  '*.test.js' -delete`) and a drift check (`diff -rq web internal/webui/web
  | grep -v '\.test\.js'` prints nothing) before the task is done
  (`docker-operator/CLAUDE.md`).

## Review Focus

1. A partial Stop/Resume failure (one of two `ContainerStop` calls fails, or
   any step mid-rebuild fails) lands `StatusError`, never stuck in
   `StatusUpdating` — a reasonable person expects a clear, actionable
   failure state, not a record frozen mid-operation.
2. Two requests racing for the same agent ID (a double-click, or a Stop
   racing a concurrent Update) get a clean `operation_in_flight` 409 via the
   shared `inFlight` registry, never two sets of Docker calls racing each
   other against the same containers.
3. `ResumeAgent` succeeds with no `Template` lookup at all — a resumed agent
   whose originating Template was since deleted must still come back up,
   since `store.Agent` alone already carries everything the rebuild needs.
4. `wakeAgent`'s existing hot path — a healthy running agent, which
   `Reconcile` checks every tick — must still make zero store writes and
   zero rebuild calls after Task 1's extraction. This is exactly the kind of
   regression a refactor silently introduces; `TestReconcile_HealthyRunningAgent_SurvivesUntouched`
   is the existing pin for it and must keep passing completely unmodified.
5. Calling Stop or Resume on an agent ID that does not exist at all returns
   404, not a panic or a leaked `inFlight` registration — `defer
   m.inFlight.release(id)` must fire on every return path, including the
   very first one (agent not found).

---

### Task 1: Generalize the shield and extract a shared rebuild helper

This is a pure refactor of already-shipped, already-tested code
(`internal/agent/reconcile.go`'s `wakeAgent` and its two private helpers). It
introduces no new public behavior — Task 2 is the first caller of the new
pieces outside `wakeAgent` itself. Its job is to make `wakeAgent`'s
container-rebuild sequence and its "is this record still in the status I
expect" shield reusable by `StopAgent`/`ResumeAgent` (Task 2), without
changing one byte of `wakeAgent`'s own observable behavior — including its
early-return/no-op path when nothing needs waking, which is the single
easiest thing for this kind of extraction to quietly break.

**Files:**
- Modify: `internal/agent/reconcile.go` (renames `markWakeUpdating` →
  `markShielded`, `errWakeStandDown` → `errShieldStandDown`, `markWakeError`
  → `markRebuildError`; extracts `rebuildAgentContainer`; updates
  `wakeAgent`'s tail to call the renamed/extracted pieces)
- Modify: `internal/agent/reconcile_test.go` (renames `TestMarkWakeUpdating`
  → `TestMarkShielded`, updates its calls to the new signature, adds a new
  subtest exercising `from=StatusStopped`)

**Interfaces:**
- Consumes: `store.StatusRunning`/`store.StatusStopped`/`store.StatusUpdating`
  (`internal/store/store.go`), `m.store.Update`, `removeAgentContainer`,
  `startAgentContainer`, `waitTmuxSession`, `markRunning`, `firstInvocationArgs`,
  `sessionResume` — all pre-existing, unchanged.
- Produces (for Task 2):
  - `func (m *Manager) markShielded(ctx context.Context, id string, from store.Status) (bool, error)`
  - `func (m *Manager) markRebuildError(ctx context.Context, id string, cause error)`
  - `func (m *Manager) rebuildAgentContainer(ctx context.Context, a *store.Agent, rb resolvedBackend) error`

- [ ] **Step 1: Write a failing test for the generalized shield**

Add this test to `internal/agent/reconcile_test.go`, replacing the existing
`TestMarkWakeUpdating` function entirely (same file, same position):

```go
func TestMarkShielded(t *testing.T) {
	t.Run("a running record is moved to updating when from=Running", func(t *testing.T) {
		m, _, st := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)

		ok, err := m.markShielded(ctx, a.ID, store.StatusRunning)
		if err != nil {
			t.Fatalf("markShielded: %v", err)
		}
		if !ok {
			t.Fatal("markShielded = false, want true for a running record")
		}
		got, err := st.Get(ctx, a.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != store.StatusUpdating {
			t.Errorf("Status = %q, want %q", got.Status, store.StatusUpdating)
		}
	})

	t.Run("a stopped record is moved to updating when from=Stopped", func(t *testing.T) {
		m, _, st := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)
		if _, err := st.Update(ctx, a.ID, func(ag *store.Agent) error {
			ag.Status = store.StatusStopped
			return nil
		}); err != nil {
			t.Fatalf("seeding stopped: %v", err)
		}

		ok, err := m.markShielded(ctx, a.ID, store.StatusStopped)
		if err != nil {
			t.Fatalf("markShielded: %v", err)
		}
		if !ok {
			t.Fatal("markShielded = false, want true for a stopped record with from=Stopped")
		}
		got, err := st.Get(ctx, a.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != store.StatusUpdating {
			t.Errorf("Status = %q, want %q", got.Status, store.StatusUpdating)
		}
	})

	t.Run("a record no longer matching from stands the shield down untouched", func(t *testing.T) {
		m, _, st := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)
		if _, err := st.Update(ctx, a.ID, func(ag *store.Agent) error {
			ag.Status = store.StatusError
			ag.ErrorMessage = "set by another operation"
			return nil
		}); err != nil {
			t.Fatalf("racing the shield: %v", err)
		}

		ok, err := m.markShielded(ctx, a.ID, store.StatusRunning)
		if err != nil {
			t.Fatalf("markShielded: %v", err)
		}
		if ok {
			t.Fatal("markShielded = true, want false (the record no longer matches from)")
		}
		got, err := st.Get(ctx, a.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != store.StatusError || got.ErrorMessage != "set by another operation" {
			t.Errorf("record = %+v, want it untouched by the losing shield", got)
		}
	})

	t.Run("a missing record is an error, not a silent stand-down", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		if _, err := m.markShielded(context.Background(), "agt_missing", store.StatusRunning); !store.IsNotFound(err) {
			t.Errorf("markShielded(missing) err = %v, want store.IsNotFound", err)
		}
	})
}
```

- [ ] **Step 2: Run the test to confirm it fails to compile**

Run (from `docker-operator/`):
```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator -e GOTOOLCHAIN=auto \
  -e GOCACHE=/work/.gocache -e GOPATH=/work/.gopath golang:1.25 \
  go test ./internal/agent/... -run TestMarkShielded -v
```
Expected: FAIL — `m.markShielded` is undefined (the production code still
only has `markWakeUpdating`, with the old, unparameterized signature).

- [ ] **Step 3: Rename and generalize the shield in reconcile.go**

In `internal/agent/reconcile.go`, replace `errWakeStandDown`'s declaration
and `markWakeUpdating`'s definition (currently just above `wakeAgent`'s doc
comment, around where the earlier read of this file placed them) with:

```go
// errShieldStandDown is the internal sentinel markShielded's mutator returns
// to abort its store write (rolling the transaction back) when the record no
// longer matches the expected starting status. It never escapes this
// package: markShielded maps it to (false, nil).
var errShieldStandDown = errors.New("the record no longer matches the expected status")

// markShielded transitions an agent record from status `from` to
// StatusUpdating -- the device that makes a container recreate safe to run
// concurrently with Reconcile and the status-sync events goroutine: while the
// record says Updating, the old container's own die/stop event hits
// MarkUnexpectedExit's "not running" guard and no-ops.
//
// It reports ok=false, with a nil error, when the record was no longer
// `from` inside the store.Update transaction -- another operation already
// won the race between the caller listing the record and this write.
//
// Shared by wakeAgent (from=StatusRunning), StopAgent (from=StatusRunning)
// and ResumeAgent (from=StatusStopped).
func (m *Manager) markShielded(ctx context.Context, id string, from store.Status) (bool, error) {
	_, err := m.store.Update(ctx, id, func(ag *store.Agent) error {
		if ag.Status != from {
			return errShieldStandDown
		}
		ag.Status = store.StatusUpdating
		ag.ErrorMessage = ""
		return nil
	})
	switch {
	case errors.Is(err, errShieldStandDown):
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}
```

- [ ] **Step 4: Generalize the error-marking helper**

Replace `markWakeError`'s definition with:

```go
// markRebuildError settles a failed container rebuild the same way
// failUpdate settles a failed in-place update: StatusError with an
// explanatory message, no container ID (it may already be gone), and every
// volume left exactly as it is. Logged, not returned -- the caller already
// has the error that matters, and this write itself uses a context detached
// from ctx so it still lands even when ctx is what is failing.
//
// Shared by wakeAgent and ResumeAgent -- both recreate the agent container
// to resume its previous session, and both fail the same way.
func (m *Manager) markRebuildError(ctx context.Context, id string, cause error) {
	if _, err := m.store.Update(context.WithoutCancel(ctx), id, func(ag *store.Agent) error {
		ag.Status = store.StatusError
		ag.ErrorMessage = "could not bring this agent's container up (its workspace, Claude-config and DinD-cache volumes are intact -- fix the cause and retry): " + cause.Error()
		ag.ContainerID = ""
		return nil
	}); err != nil {
		m.log.ErrorContext(ctx, "could not record a failed rebuild attempt; the record may no longer reflect reality",
			"agent_id", id, "error", err)
	}
}
```

- [ ] **Step 5: Extract the shared rebuild helper and update `wakeAgent`'s tail**

Add this new function in `internal/agent/reconcile.go`, right after
`markRebuildError`:

```go
// rebuildAgentContainer stops and removes a's own container and starts a
// fresh one with firstInvocationArgs(a, sessionResume), so the harness
// resumes the agent's previous conversation rather than booting fresh. The
// caller must already hold the shield (markShielded succeeded) before
// calling this -- it does not take or check one itself.
//
// Shared by wakeAgent (recovering a StatusRunning agent whose container
// died) and ResumeAgent (bringing a StatusStopped agent back up). Any
// failure here marks the record StatusError itself via markRebuildError, so
// neither caller needs to.
func (m *Manager) rebuildAgentContainer(ctx context.Context, a *store.Agent, rb resolvedBackend) error {
	if err := m.removeAgentContainer(ctx, *a); err != nil {
		err = fmt.Errorf("removing the old agent container: %w", err)
		m.markRebuildError(ctx, a.ID, err)
		return err
	}
	if err := m.startAgentContainer(ctx, a, rb, firstInvocationArgs(*a, sessionResume)...); err != nil {
		err = fmt.Errorf("starting the agent container: %w", err)
		m.markRebuildError(ctx, a.ID, err)
		return err
	}
	if err := m.waitTmuxSession(ctx, *a); err != nil {
		m.markRebuildError(ctx, a.ID, err)
		return err
	}
	if err := m.markRunning(ctx, a); err != nil {
		err = fmt.Errorf("marking the agent running after the rebuild: %w", err)
		m.markRebuildError(ctx, a.ID, err)
		return err
	}
	return nil
}
```

Then, in `wakeAgent`, replace everything from its `markWakeUpdating` call
through its final `markRunning` call (i.e. the whole shielded tail) with:

```go
	ok, err := m.markShielded(ctx, a.ID, store.StatusRunning)
	if err != nil {
		err = fmt.Errorf("marking the agent updating before recreating its container: %w", err)
		m.markRebuildError(ctx, a.ID, err)
		return false, err
	}
	if !ok {
		m.log.InfoContext(ctx, "the agent is no longer running; standing down from the wake-up and leaving the record to whichever operation won",
			"agent_id", a.ID)
		return false, nil
	}

	if err := m.rebuildAgentContainer(ctx, &a, rb); err != nil {
		return false, err
	}
	return true, nil
```

Every call to `m.markWakeError` earlier in `wakeAgent` (the dind-sidecar
failure, the container-inspect failure, the resolveBackendFromAgent failure
— all BEFORE the shield) becomes `m.markRebuildError`, unchanged otherwise.
Nothing before the shield call — `syncDependaproxyDinernetIP`,
`ensureDindRunning`, the `ContainerInspect`+early-return switch,
`resolveBackendFromAgent` — moves or changes; this is exactly what keeps the
hot path (the `case c.State == dockerclient.StateRunning` branch, which
returns before ever reaching the shield) a true no-op, zero store writes,
zero calls into `rebuildAgentContainer`.

- [ ] **Step 6: Run the new test, then the whole package, to confirm nothing broke**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator -e GOTOOLCHAIN=auto \
  -e GOCACHE=/work/.gocache -e GOPATH=/work/.gopath golang:1.25 \
  go test ./internal/agent/... -run TestMarkShielded -v
```
Expected: PASS (all four subtests).

Then the full package, with special attention to the three tests this
extraction must not disturb at all:
```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator -e GOTOOLCHAIN=auto \
  -e GOCACHE=/work/.gocache -e GOPATH=/work/.gopath golang:1.25 \
  go test ./internal/agent/... -v -run 'TestReconcile_HealthyRunningAgent_SurvivesUntouched|TestWakeAgent_LosesRace_StandsDown|TestReconcile_WakeLeavesRecordRunningAndReleasesRegistry|TestResolveBackendFromAgent_PreservesRecordsOwnPin'
```
Expected: PASS, all four — these are the regression guard for this task.
Then run the full suite (`go test ./internal/agent/...`) and confirm 100%
green with no other file changed.

- [ ] **Step 7: Commit**

```bash
cd /workspace/ai-sandbox
git add docker-operator/internal/agent/reconcile.go docker-operator/internal/agent/reconcile_test.go
git commit -m "agent: generalize wakeAgent's shield and extract a shared rebuild helper"
```

---

### Task 2: StopAgent and ResumeAgent

**Files:**
- Create: `internal/agent/stopresume.go`
- Create: `internal/agent/stopresume_test.go`

**Interfaces:**
- Consumes (from Task 1): `m.markShielded(ctx, id, from)`,
  `m.markRebuildError(ctx, id, cause)`,
  `m.rebuildAgentContainer(ctx, a *store.Agent, rb resolvedBackend) error`.
  Also consumes pre-existing `m.inFlight.tryBegin`/`.release`,
  `ErrOperationInFlight`, `m.store.Get`, `m.ensureDindRunning(ctx, a store.Agent) (bool, error)`,
  `m.resolveBackendFromAgent(ctx, a store.Agent) (resolvedBackend, error)`,
  `firstNonEmpty`, `agentContainerName(id)`, `dindContainerName(id)`,
  `m.docker.ContainerStop(ctx, ref, m.opts.StopTimeout)`, `m.stamp`.
- Produces (for Task 4):
  - `func (m *Manager) StopAgent(ctx context.Context, id string) (store.Agent, error)`
  - `func (m *Manager) ResumeAgent(ctx context.Context, id string) (store.Agent, error)`
  - `var ErrNotStoppable error` / `func IsNotStoppable(err error) bool`
  - `var ErrNotResumable error` / `func IsNotResumable(err error) bool`

- [ ] **Step 1: Write the failing tests**

Create `internal/agent/stopresume_test.go`:

```go
package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

func TestStopAgent_StopsBothContainers(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)

	got, err := m.StopAgent(ctx, a.ID)
	if err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	if got.Status != store.StatusStopped {
		t.Errorf("returned Status = %q, want %q", got.Status, store.StatusStopped)
	}

	stored, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != store.StatusStopped {
		t.Errorf("stored Status = %q, want %q", stored.Status, store.StatusStopped)
	}

	c, err := f.ContainerInspect(ctx, a.ContainerID)
	if err != nil {
		t.Fatalf("ContainerInspect(agent): %v", err)
	}
	if c.State != dockerclient.StateExited {
		t.Errorf("agent container state = %v, want exited", c.State)
	}
	d, err := f.ContainerInspect(ctx, a.DindContainerID)
	if err != nil {
		t.Fatalf("ContainerInspect(dind): %v", err)
	}
	if d.State != dockerclient.StateExited {
		t.Errorf("dind container state = %v, want exited", d.State)
	}
}

func TestStopAgent_RejectsNonRunning(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)
	if _, err := st.Update(ctx, a.ID, func(ag *store.Agent) error {
		ag.Status = store.StatusStopped
		return nil
	}); err != nil {
		t.Fatalf("seeding stopped: %v", err)
	}

	if _, err := m.StopAgent(ctx, a.ID); !IsNotStoppable(err) {
		t.Errorf("StopAgent(stopped agent) err = %v, want IsNotStoppable", err)
	}
}

// TestStopAgent_PartialFailureLandsError is a Review Focus case: if one of
// the two ContainerStop calls fails, the record must land StatusError, not
// stay stuck in StatusUpdating.
func TestStopAgent_PartialFailureLandsError(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)
	f.FailOnce(dockerclienttest.OpContainerStop, errors.New("boom"))

	if _, err := m.StopAgent(ctx, a.ID); err == nil {
		t.Fatal("StopAgent = nil error, want the injected failure")
	}

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusError {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusError)
	}
	if got.ContainerID == "" {
		t.Error("ContainerID cleared, want it left intact -- Stop never removes anything")
	}
}

// TestStopAgent_OperationInFlight is a Review Focus case: a second
// Stop/Resume/Update/Delete for the same agent ID while one is already
// running must 409, not race Docker calls.
func TestStopAgent_OperationInFlight(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)
	if !m.inFlight.tryBegin(a.ID) {
		t.Fatal("tryBegin: want true on a fresh agent")
	}
	defer m.inFlight.release(a.ID)

	if _, err := m.StopAgent(ctx, a.ID); !IsOperationInFlight(err) {
		t.Errorf("StopAgent err = %v, want IsOperationInFlight", err)
	}
}

// TestStopAgent_MissingAgentReleasesInFlight is a Review Focus case: Stop on
// a nonexistent agent must still release the in-flight registration it took,
// not leak it.
func TestStopAgent_MissingAgentReleasesInFlight(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	if _, err := m.StopAgent(context.Background(), "agt_missing"); !store.IsNotFound(err) {
		t.Errorf("StopAgent(missing) err = %v, want store.IsNotFound", err)
	}
	if m.inFlight.busy("agt_missing") {
		t.Error("in-flight registry still holds agt_missing after a not-found Stop")
	}
}

// TestResumeAgent_MissingAgentReleasesInFlight mirrors
// TestStopAgent_MissingAgentReleasesInFlight: Resume on a nonexistent agent
// must still release the in-flight registration it took, not leak it.
func TestResumeAgent_MissingAgentReleasesInFlight(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	if _, err := m.ResumeAgent(context.Background(), "agt_missing"); !store.IsNotFound(err) {
		t.Errorf("ResumeAgent(missing) err = %v, want store.IsNotFound", err)
	}
	if m.inFlight.busy("agt_missing") {
		t.Error("in-flight registry still holds agt_missing after a not-found Resume")
	}
}

func TestResumeAgent_RebuildsBothContainers(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)
	if _, err := m.StopAgent(ctx, a.ID); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	before := len(f.Calls())
	got, err := m.ResumeAgent(ctx, a.ID)
	if err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("returned Status = %q, want %q", got.Status, store.StatusRunning)
	}
	stored, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != store.StatusRunning {
		t.Errorf("stored Status = %q, want %q", stored.Status, store.StatusRunning)
	}
	created := callsAfter(f, before)
	if len(created) == 0 {
		t.Fatal("no docker calls recorded during ResumeAgent, want at least a container create")
	}
	c, err := f.ContainerInspect(ctx, stored.ContainerID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if c.State != dockerclient.StateRunning {
		t.Errorf("agent container state = %v, want running", c.State)
	}
}

func TestResumeAgent_RejectsNonStopped(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)

	if _, err := m.ResumeAgent(ctx, a.ID); !IsNotResumable(err) {
		t.Errorf("ResumeAgent(running agent) err = %v, want IsNotResumable", err)
	}
}

// TestResumeAgent_RebuildFailureLandsError is a Review Focus case, mirroring
// StopAgent's partial-failure test: a failure partway through the rebuild
// (here, the agent container never comes back up healthy) must land
// StatusError via markRebuildError, not leave the record StatusUpdating.
func TestResumeAgent_RebuildFailureLandsError(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)
	if _, err := m.StopAgent(ctx, a.ID); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	f.FailOnce(dockerclienttest.OpContainerCreate, errors.New("boom"))

	if _, err := m.ResumeAgent(ctx, a.ID); err == nil {
		t.Fatal("ResumeAgent = nil error, want the injected failure")
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusError {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusError)
	}
}

func TestResumeAgent_OperationInFlight(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)
	if _, err := st.Update(ctx, a.ID, func(ag *store.Agent) error {
		ag.Status = store.StatusStopped
		return nil
	}); err != nil {
		t.Fatalf("seeding stopped: %v", err)
	}
	if !m.inFlight.tryBegin(a.ID) {
		t.Fatal("tryBegin: want true")
	}
	defer m.inFlight.release(a.ID)

	if _, err := m.ResumeAgent(ctx, a.ID); !IsOperationInFlight(err) {
		t.Errorf("ResumeAgent err = %v, want IsOperationInFlight", err)
	}
}

// TestResumeAgent_NoTemplateLookup is a Review Focus case: ResumeAgent must
// succeed purely from the agent record -- it never looks up a Template, so
// an agent whose originating Template was since deleted still resumes fine.
// There is no Template involved in this test at all (createRunningAgent uses
// Manager.Create directly, never a CronAgent/Template), which is itself the
// proof: if ResumeAgent required a Template lookup, this test would need to
// seed one, and it does not.
func TestResumeAgent_NoTemplateLookup(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)
	if _, err := m.StopAgent(ctx, a.ID); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	if _, err := m.ResumeAgent(ctx, a.ID); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}
}

// TestResumeAgent_AfterCronStopPolicy is a Review Focus case pinning spec
// decision 3: ResumeAgent is uniform -- it must bring back an agent a cron
// agent's on_complete:"stop" policy stopped exactly the same way it brings
// back a manually-stopped one, with no "reason" distinction anywhere. This
// uses newTestCronAgent/CreateFromCronAgent/ApplyOnComplete, all already
// shipped by the merged CronAgent feature this plan builds on top of.
func TestResumeAgent_AfterCronStopPolicy(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()

	ca := newTestCronAgent(t, m, config.HarnessClaudeCode, config.BackendOllama, "x", "stop")
	a, err := m.CreateFromCronAgent(ctx, ca)
	if err != nil {
		t.Fatalf("CreateFromCronAgent: %v", err)
	}
	if err := m.ApplyOnComplete(ctx, ca, store.ScheduleRun{AgentID: a.ID}); err != nil {
		t.Fatalf("ApplyOnComplete: %v", err)
	}

	got, err := m.ResumeAgent(ctx, a.ID)
	if err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusRunning)
	}
	stored, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != store.StatusRunning {
		t.Errorf("stored Status = %q, want %q", stored.Status, store.StatusRunning)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator -e GOTOOLCHAIN=auto \
  -e GOCACHE=/work/.gocache -e GOPATH=/work/.gopath golang:1.25 \
  go test ./internal/agent/... -run 'TestStopAgent|TestResumeAgent' -v
```
Expected: FAIL to compile — `m.StopAgent`, `m.ResumeAgent`, `IsNotStoppable`,
`IsNotResumable` are all undefined.

- [ ] **Step 3: Implement StopAgent/ResumeAgent**

Create `internal/agent/stopresume.go`:

```go
package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// ErrNotStoppable is returned by StopAgent when the agent is not running.
var ErrNotStoppable = errors.New("agent is not in a stoppable state")

// IsNotStoppable reports whether err was caused by a Stop attempt against an
// agent that is not running.
func IsNotStoppable(err error) bool { return errors.Is(err, ErrNotStoppable) }

// ErrNotResumable is returned by ResumeAgent when the agent is not stopped.
var ErrNotResumable = errors.New("agent is not in a resumable state")

// IsNotResumable reports whether err was caused by a Resume attempt against
// an agent that is not stopped.
func IsNotResumable(err error) bool { return errors.Is(err, ErrNotResumable) }

// StopAgent stops a running agent's containers -- the agent container and
// its DinD sidecar -- without removing anything. The record, its volumes,
// and its container IDs are left exactly as they are; only their
// daemon-side running state changes, so ResumeAgent can bring the same
// agent back up later.
//
// It refuses to start if another operation already holds this agent in this
// process (ErrOperationInFlight, a 409), exactly like Update and Delete.
func (m *Manager) StopAgent(ctx context.Context, id string) (store.Agent, error) {
	if !m.inFlight.tryBegin(id) {
		return store.Agent{}, fmt.Errorf("stopping agent %q: %w", id, ErrOperationInFlight)
	}
	defer m.inFlight.release(id)

	a, err := m.store.Get(ctx, id)
	if err != nil {
		return store.Agent{}, err
	}
	if a.Status != store.StatusRunning {
		return store.Agent{}, fmt.Errorf("stopping agent %q: %w (status %q)", id, ErrNotStoppable, a.Status)
	}

	ok, err := m.markShielded(ctx, id, store.StatusRunning)
	if err != nil {
		return store.Agent{}, fmt.Errorf("stopping agent %q: marking it updating: %w", id, err)
	}
	if !ok {
		return store.Agent{}, fmt.Errorf("stopping agent %q: %w (status changed concurrently)", id, ErrNotStoppable)
	}

	agentRef := firstNonEmpty(a.ContainerID, a.ContainerName, agentContainerName(a.ID))
	dindRef := firstNonEmpty(a.DindContainerID, a.DindContainerName, dindContainerName(a.ID))
	var errs []error
	if err := m.docker.ContainerStop(ctx, agentRef, m.opts.StopTimeout); err != nil {
		errs = append(errs, fmt.Errorf("stopping the agent container %q: %w", agentRef, err))
	}
	if err := m.docker.ContainerStop(ctx, dindRef, m.opts.StopTimeout); err != nil {
		errs = append(errs, fmt.Errorf("stopping the dind sidecar %q: %w", dindRef, err))
	}
	if err := errors.Join(errs...); err != nil {
		m.markStopError(ctx, id, err)
		return store.Agent{}, err
	}

	if err := m.markStopped(ctx, &a); err != nil {
		return store.Agent{}, fmt.Errorf("stopping agent %q: marking it stopped: %w", id, err)
	}
	return a, nil
}

// markStopped is StopAgent's success step: both containers are confirmed
// stopped, so the record becomes StatusStopped, closing the shield opened by
// markShielded.
func (m *Manager) markStopped(ctx context.Context, a *store.Agent) error {
	return m.stamp(ctx, a, "the stopped status", func(ag *store.Agent) {
		ag.Status = store.StatusStopped
		ag.ErrorMessage = ""
	})
}

// markStopError settles a failed Stop attempt: StatusError with an
// explanatory message. Unlike markRebuildError, nothing was removed here --
// Stop only ever calls ContainerStop, never ContainerRemove -- so container
// IDs and volumes are left completely untouched, and the message says so.
func (m *Manager) markStopError(ctx context.Context, id string, cause error) {
	if _, err := m.store.Update(context.WithoutCancel(ctx), id, func(ag *store.Agent) error {
		ag.Status = store.StatusError
		ag.ErrorMessage = "could not stop this agent (its containers, workspace, Claude-config and DinD-cache volumes are all intact -- fix the cause and retry): " + cause.Error()
		return nil
	}); err != nil {
		m.log.ErrorContext(ctx, "could not record a failed stop attempt; the record may no longer reflect reality",
			"agent_id", id, "error", err)
	}
}

// ResumeAgent brings a stopped agent back to StatusRunning. It is not a
// plain `docker start`: the agent container is recreated with
// firstInvocationArgs(a, sessionResume), exactly like wakeAgent's own
// recovery, because a stopped container's session does not survive a plain
// restart any better than it survives a recreate.
//
// It refuses to start if another operation already holds this agent in this
// process (ErrOperationInFlight, a 409), exactly like Update and Delete.
func (m *Manager) ResumeAgent(ctx context.Context, id string) (store.Agent, error) {
	if !m.inFlight.tryBegin(id) {
		return store.Agent{}, fmt.Errorf("resuming agent %q: %w", id, ErrOperationInFlight)
	}
	defer m.inFlight.release(id)

	a, err := m.store.Get(ctx, id)
	if err != nil {
		return store.Agent{}, err
	}
	if a.Status != store.StatusStopped {
		return store.Agent{}, fmt.Errorf("resuming agent %q: %w (status %q)", id, ErrNotResumable, a.Status)
	}

	if _, err := m.ensureDindRunning(ctx, a); err != nil {
		err = fmt.Errorf("the dind sidecar: %w", err)
		m.markRebuildError(ctx, a.ID, err)
		return store.Agent{}, err
	}

	rb, err := m.resolveBackendFromAgent(ctx, a)
	if err != nil {
		err = fmt.Errorf("resolving the backend to restart the agent container: %w", err)
		m.markRebuildError(ctx, a.ID, err)
		return store.Agent{}, err
	}

	ok, err := m.markShielded(ctx, id, store.StatusStopped)
	if err != nil {
		err = fmt.Errorf("marking the agent updating before resuming its container: %w", err)
		m.markRebuildError(ctx, id, err)
		return store.Agent{}, err
	}
	if !ok {
		return store.Agent{}, fmt.Errorf("resuming agent %q: %w (status changed concurrently)", id, ErrNotResumable)
	}

	if err := m.rebuildAgentContainer(ctx, &a, rb); err != nil {
		return store.Agent{}, err
	}
	return a, nil
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator -e GOTOOLCHAIN=auto \
  -e GOCACHE=/work/.gocache -e GOPATH=/work/.gopath golang:1.25 \
  go test ./internal/agent/... -v -run 'TestStopAgent|TestResumeAgent'
```
Expected: PASS, all cases. Then run `go test ./internal/agent/...` in full
and confirm the whole package is still green (this also re-proves Task 1's
regression guard still holds, now with real callers of `markShielded`/
`rebuildAgentContainer` beside `wakeAgent`).

- [ ] **Step 5: Commit**

```bash
cd /workspace/ai-sandbox
git add docker-operator/internal/agent/stopresume.go docker-operator/internal/agent/stopresume_test.go
git commit -m "agent: add StopAgent and ResumeAgent"
```

---

### Task 3: Stop the DinD sidecar in ApplyOnComplete's "stop" case too

**Files:**
- Modify: `internal/agent/schedule.go:111-128` (`ApplyOnComplete`)
- Modify: `internal/agent/completion_test.go` (`TestApplyOnComplete_Stop`)

**Interfaces:**
- Consumes: `firstNonEmpty`, `dindContainerName(id)` (both already used
  elsewhere in this package), `m.opts.StopTimeout`.
- Produces: no new exported surface; this is a one-line behavior change to
  already-shipped code.

- [ ] **Step 1: Extend the failing test**

In `internal/agent/completion_test.go`, extend `TestApplyOnComplete_Stop`
(replace its full body) to also assert the sidecar stopped:

```go
func TestApplyOnComplete_Stop(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	ca := newTestCronAgent(t, m, config.HarnessClaudeCode, config.BackendOllama, "x", "stop")
	a, err := m.CreateFromCronAgent(ctx, ca)
	if err != nil {
		t.Fatalf("CreateFromCronAgent: %v", err)
	}
	run := store.ScheduleRun{AgentID: a.ID}
	if err := m.ApplyOnComplete(ctx, ca, run); err != nil {
		t.Fatalf("ApplyOnComplete: %v", err)
	}

	c, err := f.ContainerInspect(ctx, agentContainerName(a.ID))
	if err != nil {
		t.Fatalf("ContainerInspect(agent): %v", err)
	}
	if c.State != dockerclient.StateExited {
		t.Errorf("agent container state = %v, want exited (stopped)", c.State)
	}
	d, err := f.ContainerInspect(ctx, dindContainerName(a.ID))
	if err != nil {
		t.Fatalf("ContainerInspect(dind): %v", err)
	}
	if d.State != dockerclient.StateExited {
		t.Errorf("dind sidecar state = %v, want exited (stopped) -- on_complete:\"stop\" must stop the sidecar too", d.State)
	}
}
```

- [ ] **Step 2: Run the test to confirm it fails**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator -e GOTOOLCHAIN=auto \
  -e GOCACHE=/work/.gocache -e GOPATH=/work/.gopath golang:1.25 \
  go test ./internal/agent/... -run TestApplyOnComplete_Stop -v
```
Expected: FAIL — the sidecar is still running (`dockerclient.StateRunning`),
since `ApplyOnComplete` today only stops the agent container.

- [ ] **Step 3: Stop the sidecar too**

In `internal/agent/schedule.go`, replace `ApplyOnComplete`'s `"stop"` case
(the one that currently does `m.store.Update(...)` then a single
`m.docker.ContainerStop(ctx, agentContainerName(run.AgentID), m.opts.StopTimeout)`)
with:

```go
	case "stop":
		if _, err := m.store.Update(ctx, run.AgentID, func(a *store.Agent) error {
			a.Status = store.StatusStopped
			return nil
		}); err != nil {
			return err
		}
		var errs []error
		if err := m.docker.ContainerStop(ctx, agentContainerName(run.AgentID), m.opts.StopTimeout); err != nil {
			errs = append(errs, err)
		}
		if err := m.docker.ContainerStop(ctx, dindContainerName(run.AgentID), m.opts.StopTimeout); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
```

`schedule.go`'s import block does not currently import `"errors"` — add it,
alphabetically among the standard-library imports (`"context"`, `"errors"`,
`"io"`, `"strings"`, `"time"`).

- [ ] **Step 4: Run the test to confirm it passes**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator -e GOTOOLCHAIN=auto \
  -e GOCACHE=/work/.gocache -e GOPATH=/work/.gopath golang:1.25 \
  go test ./internal/agent/... -v -run 'TestApplyOnComplete'
```
Expected: PASS for all `TestApplyOnComplete_*` tests, including the extended
one and the untouched `_Delete`/`_Leave`/`_StopShieldsRecord` siblings.

- [ ] **Step 5: Commit**

```bash
cd /workspace/ai-sandbox
git add docker-operator/internal/agent/schedule.go docker-operator/internal/agent/completion_test.go
git commit -m "agent: stop the dind sidecar too in ApplyOnComplete's stop policy"
```

---

### Task 4: API routes

**Files:**
- Modify: `internal/api/handlers.go` (`AgentManager` interface, two new
  routes, two new handlers, two new `methodNotAllowed` registrations)
- Modify: `internal/api/errors.go` (`CodeNotStoppable`, `CodeNotResumable`)
- Modify: `internal/api/handlers_test.go` (`fakeManager` gains
  `StopAgent`/`ResumeAgent`, plus the new handler tests)

**Interfaces:**
- Consumes (from Task 2): `agent.ErrNotStoppable`/`agent.IsNotStoppable`,
  `agent.ErrNotResumable`/`agent.IsNotResumable`, `agent.IsOperationInFlight`
  (already consumed elsewhere in this file), `Manager.StopAgent(ctx, id) (store.Agent, error)`,
  `Manager.ResumeAgent(ctx, id) (store.Agent, error)`.
- Produces: `POST /api/agents/{id}/stop`, `POST /api/agents/{id}/resume`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/api/handlers_test.go`, near the existing
`TestHandleUpdate_*`/`TestHandleDelete_OperationInFlight` tests:

```go
func TestHandleStopAgent_OK(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Status: store.StatusRunning})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_1/stop", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	if a := decodeAgent(t, rec); a.Status != store.StatusStopped {
		t.Errorf("returned agent status = %q, want %q", a.Status, store.StatusStopped)
	}
	if len(mgr.stopCalls) != 1 || mgr.stopCalls[0] != "agt_1" {
		t.Errorf("stopCalls = %v, want [\"agt_1\"]", mgr.stopCalls)
	}
}

func TestHandleStopAgent_NotFound(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_missing/stop", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body)
	}
}

func TestHandleStopAgent_NotStoppable(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Status: store.StatusStopped})
	mgr.stopErr = fmt.Errorf("stopping agent %q: %w", "agt_1", agent.ErrNotStoppable)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_1/stop", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeNotStoppable {
		t.Errorf("error code = %q, want %q", got, CodeNotStoppable)
	}
}

func TestHandleStopAgent_OperationInFlight(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Status: store.StatusRunning})
	mgr.stopErr = fmt.Errorf("stopping agent %q: %w", "agt_1", agent.ErrOperationInFlight)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_1/stop", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeOperationInFlight {
		t.Errorf("error code = %q, want %q", got, CodeOperationInFlight)
	}
}

func TestHandleStopAgent_MethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_1/stop", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusMethodNotAllowed, rec.Body)
	}
}

func TestHandleResumeAgent_OK(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Status: store.StatusStopped})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_1/resume", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	if a := decodeAgent(t, rec); a.Status != store.StatusRunning {
		t.Errorf("returned agent status = %q, want %q", a.Status, store.StatusRunning)
	}
	if len(mgr.resumeCalls) != 1 || mgr.resumeCalls[0] != "agt_1" {
		t.Errorf("resumeCalls = %v, want [\"agt_1\"]", mgr.resumeCalls)
	}
}

func TestHandleResumeAgent_NotFound(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_missing/resume", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body)
	}
}

func TestHandleResumeAgent_NotResumable(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Status: store.StatusRunning})
	mgr.resumeErr = fmt.Errorf("resuming agent %q: %w", "agt_1", agent.ErrNotResumable)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_1/resume", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeNotResumable {
		t.Errorf("error code = %q, want %q", got, CodeNotResumable)
	}
}

func TestHandleResumeAgent_OperationInFlight(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Status: store.StatusStopped})
	mgr.resumeErr = fmt.Errorf("resuming agent %q: %w", "agt_1", agent.ErrOperationInFlight)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_1/resume", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeOperationInFlight {
		t.Errorf("error code = %q, want %q", got, CodeOperationInFlight)
	}
}

func TestHandleResumeAgent_MethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_1/resume", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusMethodNotAllowed, rec.Body)
	}
}
```

Add `fakeManager` fields and methods, next to `updateErr`/`updateReqs` and
`func (f *fakeManager) Update`:

```go
	stopErr     error
	stopCalls   []string
	resumeErr   error
	resumeCalls []string
```

```go
func (f *fakeManager) StopAgent(_ context.Context, id string) (store.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls = append(f.stopCalls, id)
	if f.stopErr != nil {
		return store.Agent{}, f.stopErr
	}
	a, ok := f.agents[id]
	if !ok {
		return store.Agent{}, fmt.Errorf("stopping agent %q: %w", id, store.ErrNotFound)
	}
	a.Status = store.StatusStopped
	f.agents[id] = a
	return a, nil
}

func (f *fakeManager) ResumeAgent(_ context.Context, id string) (store.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls = append(f.resumeCalls, id)
	if f.resumeErr != nil {
		return store.Agent{}, f.resumeErr
	}
	a, ok := f.agents[id]
	if !ok {
		return store.Agent{}, fmt.Errorf("resuming agent %q: %w", id, store.ErrNotFound)
	}
	a.Status = store.StatusRunning
	f.agents[id] = a
	return a, nil
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator -e GOTOOLCHAIN=auto \
  -e GOCACHE=/work/.gocache -e GOPATH=/work/.gopath golang:1.25 \
  go test ./internal/api/... -run 'TestHandleStopAgent|TestHandleResumeAgent' -v
```
Expected: FAIL to compile — `fakeManager` does not yet implement
`StopAgent`/`ResumeAgent`, the routes do not exist, and `CodeNotStoppable`/
`CodeNotResumable` are undefined.

- [ ] **Step 3: Add the error codes, interface methods, routes and handlers**

In `internal/api/errors.go`, add after `CodeTemplateInUse`:

```go
	// CodeNotStoppable is returned (409) by POST /api/agents/{id}/stop when
	// the agent is not in a stoppable state (only a running agent can be
	// stopped).
	CodeNotStoppable = "not_stoppable"
	// CodeNotResumable is returned (409) by POST /api/agents/{id}/resume when
	// the agent is not in a resumable state (only a stopped agent can be
	// resumed).
	CodeNotResumable = "not_resumable"
```

In `internal/api/handlers.go`, add to the `AgentManager` interface, right
after `Update`'s entry:

```go
	// StopAgent stops a running agent's containers (the agent container and
	// its DinD sidecar) without removing anything. Errors: store.IsNotFound
	// (404), agent.IsNotStoppable (409), agent.IsOperationInFlight (409).
	StopAgent(ctx context.Context, id string) (store.Agent, error)

	// ResumeAgent brings a stopped agent back to StatusRunning, recreating
	// its container with a session-resume flag. Errors: store.IsNotFound
	// (404), agent.IsNotResumable (409), agent.IsOperationInFlight (409).
	ResumeAgent(ctx context.Context, id string) (store.Agent, error)
```

Add the two routes right after `GET /api/agents/{id}/info`:

```go
	mux.HandleFunc("POST /api/agents/{id}/stop", h.handleStopAgent)
	mux.HandleFunc("POST /api/agents/{id}/resume", h.handleResumeAgent)
```

Add the two `methodNotAllowed` registrations alongside the other
`/api/agents/{id}/...` ones:

```go
	mux.HandleFunc("/api/agents/{id}/stop", methodNotAllowed)
	mux.HandleFunc("/api/agents/{id}/resume", methodNotAllowed)
```

Add the two handlers, right after `handleUpdate`:

```go
// handleStopAgent serves POST /api/agents/{id}/stop: stops a running
// agent's containers (the agent container and its DinD sidecar) without
// removing anything, so ResumeAgent can bring it back up later.
func (h *Handler) handleStopAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := h.mgr.StopAgent(r.Context(), id)
	if err != nil {
		switch {
		case store.IsNotFound(err):
			writeError(w, http.StatusNotFound, CodeNotFound, "no such agent", "")
		case agent.IsOperationInFlight(err):
			writeError(w, http.StatusConflict, CodeOperationInFlight, "another operation is already in flight for this agent; retry once it finishes", "")
		case agent.IsNotStoppable(err):
			writeError(w, http.StatusConflict, CodeNotStoppable, "only a running agent can be stopped", "")
		default:
			h.internalError(w, "stopping agent "+id, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, h.buildAgentViews(r.Context(), []store.Agent{a})[0])
}

// handleResumeAgent serves POST /api/agents/{id}/resume: brings a stopped
// agent back to StatusRunning by recreating its container with a
// session-resume flag, exactly like the reconcile pass's own automatic
// wake-up.
func (h *Handler) handleResumeAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := h.mgr.ResumeAgent(r.Context(), id)
	if err != nil {
		switch {
		case store.IsNotFound(err):
			writeError(w, http.StatusNotFound, CodeNotFound, "no such agent", "")
		case agent.IsOperationInFlight(err):
			writeError(w, http.StatusConflict, CodeOperationInFlight, "another operation is already in flight for this agent; retry once it finishes", "")
		case agent.IsNotResumable(err):
			writeError(w, http.StatusConflict, CodeNotResumable, "only a stopped agent can be resumed", "")
		default:
			h.internalError(w, "resuming agent "+id, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, h.buildAgentViews(r.Context(), []store.Agent{a})[0])
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator -e GOTOOLCHAIN=auto \
  -e GOCACHE=/work/.gocache -e GOPATH=/work/.gopath golang:1.25 \
  go test ./internal/api/... -v -run 'TestHandleStopAgent|TestHandleResumeAgent'
```
Expected: PASS, all cases. Then run `go test ./...` from
`docker-operator/` in full (via the same Docker invocation) to confirm
nothing else in the module broke, since `AgentManager` gained two methods
every other implementer of that interface must also satisfy (there should
be exactly one: `agent.Manager`, already updated by Task 2).

- [ ] **Step 5: Commit**

```bash
cd /workspace/ai-sandbox
git add docker-operator/internal/api/handlers.go docker-operator/internal/api/errors.go docker-operator/internal/api/handlers_test.go
git commit -m "api: add POST /api/agents/{id}/stop and /resume"
```

---

### Task 5: UI — Stop/Resume in the agent detail page's options menu

**Files:**
- Modify: `web/terminal.js` (`renderAgentDetail`: two new menu items, two
  new pure gating functions, two new click handlers)
- Modify: `web/terminal.test.js` (tests for the two new pure functions only
  — see the note below on what is and isn't unit-tested here)
- Regenerate: `internal/webui/web/*` (`make sync-web-embed` / the manual
  equivalent from the Global Constraints section)

**Interfaces:**
- Consumes (from Task 4): `POST /api/agents/{id}/stop`,
  `POST /api/agents/{id}/resume`, both returning the updated agent (with
  `status`) on 200.
- Consumes (pre-existing in this file): `fetchJSON`, `window.OperatorConfirm.show(message, opts)`,
  the `.detail__menu-panel`/`.detail__menu-item` DOM structure and its
  existing `data-action` dispatch in `renderAgentDetail`.
- Produces: nothing a later task needs — this is the last task.

**A note on what this task tests.** `web/terminal.test.js`'s own header
comment already states this file's established scope: the pure helpers
extracted from `terminal.js` (`resizeFrame`, `encodeKeystroke`, etc.) are
unit-tested here; `renderAgentDetail` itself — including its existing
Delete button and `update-agent` menu item — is **not**, because it needs a
real DOM/WebSocket this project deliberately has no jsdom/headless-browser
tooling for, and is covered by manual review plus an integration test
against a real running backend instead. This task follows that same split:
it extracts two small, genuinely pure gating functions and unit-tests
those, but the actual menu-item wiring (hidden-toggling, the confirm gate,
the fetch calls) is manual-review-only, exactly like the Delete button
beside it.

- [ ] **Step 1: Write the failing tests for the two pure gating functions**

Add to `web/terminal.test.js`, near the top-level `require` of the other
extracted helpers:

```js
const {
	resizeFrame,
	encodeKeystroke,
	shouldForwardWheel,
	isCopyShortcut,
	isPasteShortcut,
	terminalTextToPlain,
	buildContextHTML,
	clampToLastLines,
	replayCaptureToText,
	canStopAgent,
	canResumeAgent,
} = require('./terminal.js');
```

```js
test('canStopAgent: true only for a running agent', () => {
	assert.equal(canStopAgent('running'), true);
	assert.equal(canStopAgent('stopped'), false);
	assert.equal(canStopAgent('error'), false);
	assert.equal(canStopAgent(''), false);
});

test('canResumeAgent: true only for a stopped agent', () => {
	assert.equal(canResumeAgent('stopped'), true);
	assert.equal(canResumeAgent('running'), false);
	assert.equal(canResumeAgent(''), false);
});
```

- [ ] **Step 2: Run the tests to confirm they fail**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator node:22 node --test web/terminal.test.js
```
Expected: FAIL — `canStopAgent`/`canResumeAgent` are undefined (the
destructured `require` gives `undefined`, and calling `undefined('running')`
throws).

- [ ] **Step 3: Implement the gating functions and the menu wiring**

In `web/terminal.js`, add these two functions next to the other small pure
helpers (e.g. beside `shouldForwardWheel`):

```js
	// canStopAgent/canResumeAgent gate the detail page's Stop/Resume menu
	// items on the agent's current status -- mutually exclusive single-status
	// checks, unlike Update's own broader running/stopped/error allow-list.
	function canStopAgent(status) {
		return status === 'running';
	}

	function canResumeAgent(status) {
		return status === 'stopped';
	}
```

And export them alongside the module's other exports (find the existing
`module.exports = { ... }` block and add `canStopAgent, canResumeAgent,` to
it).

In `renderAgentDetail`'s menu HTML (inside `.detail__menu-panel`), add two
new items right after the `update-agent` button:

```html
'<button class="detail__menu-item" type="button" role="menuitem" data-action="stop-agent" hidden>Stop agent</button>' +
'<button class="detail__menu-item" type="button" role="menuitem" data-action="resume-agent" hidden>Resume agent</button>' +
```

Right after the existing `var deleteBtn = ...` line, add:

```js
		var stopItem = container.querySelector('[data-action="stop-agent"]');
		var resumeItem = container.querySelector('[data-action="resume-agent"]');
```

In the `fetchJSON('/api/agents/' + ...)` success callback (the one that
currently sets `nameInput.value`, `idEl.textContent`, etc.), add:

```js
				if (stopItem) stopItem.hidden = !canStopAgent(agent.status);
				if (resumeItem) resumeItem.hidden = !canResumeAgent(agent.status);
```

In the existing `menuPanel.querySelectorAll('.detail__menu-item').forEach(...)`
click dispatch, extend the `if (action === ...) ... else if` chain:

```js
				else if (action === 'stop-agent') stopAgent();
				else if (action === 'resume-agent') resumeAgent();
```

Add the two handler functions near `deleteBtn.addEventListener(...)`:

```js
		function stopAgent() {
			window.OperatorConfirm.show(
				'The agent\'s containers will stop; nothing is deleted and it can be resumed later.',
				{ title: 'Stop this agent?', confirmLabel: 'Stop' }
			).then(function (confirmed) {
				if (!confirmed) return;
				fetchJSON('/api/agents/' + encodeURIComponent(agentID) + '/stop', { method: 'POST' })
					.then(function () { renderAgentDetail(container, agentID); })
					.catch(function (e) { window.alert('Could not stop agent: ' + e.message); });
			});
		}

		function resumeAgent() {
			fetchJSON('/api/agents/' + encodeURIComponent(agentID) + '/resume', { method: 'POST' })
				.then(function () { renderAgentDetail(container, agentID); })
				.catch(function (e) { window.alert('Could not resume agent: ' + e.message); });
		}
```

(`window.OperatorConfirm.show` with no `danger: true` renders the plain,
non-danger styling — per spec decision 4. Both handlers re-render the whole
detail page on success, which re-fetches the agent and so re-evaluates
`canStopAgent`/`canResumeAgent` against the new status, refreshing which
item is hidden.)

- [ ] **Step 4: Run the tests to confirm they pass**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator node:22 node --test web/terminal.test.js
```
Expected: PASS, including the two new tests. Then run the full web suite:
```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work \
  -w /work/ai-sandbox/docker-operator node:22 \
  node --test web/render.test.js web/terminal.test.js web/auth.test.js web/files.test.js web/templates.test.js
```
Expected: PASS, all suites.

- [ ] **Step 5: Regenerate the embedded web copy and verify no drift**

```sh
cd /workspace/ai-sandbox/docker-operator
rm -rf internal/webui/web && mkdir -p internal/webui/web && cp -a web/. internal/webui/web/ && find internal/webui/web -name '*.test.js' -delete
diff -rq web internal/webui/web | grep -v '\.test\.js'
```
Expected: the `diff` prints nothing.

- [ ] **Step 6: Manual verification (this is the actual acceptance test for the DOM wiring)**

Since this project has no jsdom/headless-browser tooling, the one thing the
automated tests above cannot prove is the live wiring: open a running
agent's detail page and confirm the `⋮` menu shows "Stop agent" (not
"Resume agent"); click it, confirm the themed confirm dialog appears
(non-danger styling, not red); confirm; confirm the agent's containers stop
and the page re-renders showing "Resume agent" instead; click that; confirm
no dialog appears and the agent comes back up with its previous tmux
session intact (its prior terminal output is still there, not a fresh
shell).

- [ ] **Step 7: Commit**

```bash
cd /workspace/ai-sandbox
git add docker-operator/web/terminal.js docker-operator/web/terminal.test.js docker-operator/internal/webui/web
git commit -m "web: add Stop/Resume to the agent detail page's options menu"
```
