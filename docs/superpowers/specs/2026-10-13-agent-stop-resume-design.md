# Design: manual Stop / Resume for agents

**Status:** design, not yet implemented.

**Branch:** `feat/agent-stop-resume`, branched from `main` (which has #236's
CronAgent/Template split merged).

**Issue:** none filed — a direct follow-up request after a quick
architecture investigation (`We have any sleep or stop feature for agents?`).

## Why

An agent today has exactly two end states a person can put it in directly:
running (via Create) or gone (via Delete). There is no way to pause a
running agent — stop paying for its containers — without destroying it.

The plumbing for this already exists, just not exposed. `store.Status`
already defines `StatusStopped` ("the containers exist but are not
running"), and `(*Manager) ApplyOnComplete`'s `"stop"` case already reaches
it via `m.docker.ContainerStop` on a cron agent's one-shot container when
its `OnComplete` policy says so. Separately, `wakeAgent`
(`internal/agent/reconcile.go`) already proves that an agent whose container
is down can come back up *without losing its session*: it recreates the
container with `firstInvocationArgs(a, sessionResume)` so the harness
resumes (`--resume` for claude-code, `--continue` for opencode) rather than
booting fresh, because a stopped container's tmux server dies with the
container's PID 1 regardless of whether it's a plain restart or a full
recreate.

This spec wires those two pieces together into a person-initiated action:
Stop (from Running to Stopped, on demand) and Resume (from Stopped back to
Running, reusing the session-preserving rebuild `wakeAgent` already does).

## Decisions locked during brainstorming

1. **Scope: manual Stop + Resume only.** No automatic idle-timeout sleep —
   that is a separate, likely-later feature with its own design questions
   (what counts as idle, what wakes an agent back up on demand).
2. **Stop stops BOTH containers** — the agent container and its DinD
   sidecar — for the full resource saving, not just the agent container.
   This also means updating the existing `ApplyOnComplete`'s `"stop"` case
   (today agent-container-only) to stop the sidecar too, so every
   `StatusStopped` agent ends up in the same shape regardless of whether a
   person's Stop or a cron agent's `on_complete:"stop"` policy put it there.
3. **Resume is uniform.** It works on any `StatusStopped` agent, full stop —
   no "reason" field distinguishing a manual stop from a cron-policy stop.
   One status, one meaning, one resume path.
4. **Stop only applies to a `StatusRunning` agent**, and requires an
   explicit confirmation step client-side (styled as a plain confirmation,
   not Delete's danger styling — nothing is destroyed, but stopping still
   interrupts a live session, so a one-click accident is worth guarding
   against).
5. **Resume only applies to a `StatusStopped` agent**, no confirmation step
   (nothing destructive to confirm).
6. **UI surface: the agent detail page's existing `⋮` options menu**
   (`web/terminal.js`'s `renderAgentDetail`), as two new items alongside
   `view-context` / `agent-info` / `update-agent` — not a new header button,
   and not exposed on the sidebar list.
7. **Mechanism: share code with `wakeAgent`, not reimplement it.** Resume is
   never a plain `ContainerStart` on the agent container — `wakeAgent`'s own
   doc comment already explains why that would silently replay a stale
   harness arg instead of resuming. The recreate-with-resume sequence
   `wakeAgent` already runs is extracted into a shared helper both it and
   the new `ResumeAgent` call, so the two paths can't drift apart later.
8. **Concurrency: reuse `Manager.inFlight`**, the same in-process registry
   Update, Delete and reconcile's wake already share — not a new lock. A
   second Stop/Resume request for an agent ID already mid-operation gets the
   same 409 `operation_in_flight` Update/Delete already return, rather than
   racing Docker calls against each other.

## State machine

```
Running --StopAgent-->  [shielded: Updating]  --> Stopped
Stopped --ResumeAgent--> [shielded: Updating] --> Running
```

Both transitions pass through the existing `StatusUpdating` shield —
exactly the device `wakeAgent`/Update already use so the status-sync events
goroutine's `MarkUnexpectedExit` doesn't mistake the old container's
die/stop event for an unexpected crash. A failure anywhere inside the
shielded phase lands the record in `StatusError` (volumes and the record
itself always left intact — matching `failUpdate`'s and `markWakeError`'s
existing contract), never stuck in `StatusUpdating`.

Preconditions, checked before the shield is taken:
- `StopAgent`: current status must be `StatusRunning`, else `ErrNotStoppable`.
- `ResumeAgent`: current status must be `StatusStopped`, else `ErrNotResumable`.

Both acquire `m.inFlight.tryBegin(id)` first (`ErrOperationInFlight` if
busy), exactly like Update and Delete already do.

## Manager (`internal/agent`)

```go
// StopAgent stops a running agent's containers (both the agent container
// and its DinD sidecar) without removing anything. The record, its
// volumes, and its container IDs are left exactly as they are -- only
// their daemon-side running state changes -- so ResumeAgent can bring the
// same agent back up later.
func (m *Manager) StopAgent(ctx context.Context, id string) error

// ResumeAgent brings a stopped agent back to StatusRunning. It is not a
// plain `docker start`: the agent container is recreated with
// firstInvocationArgs(a, sessionResume), exactly like wakeAgent's own
// recovery, because a stopped container's session does not survive a
// plain restart any better than it survives a recreate.
func (m *Manager) ResumeAgent(ctx context.Context, id string) error
```

**Generalized shield, shared by three callers.** `markWakeUpdating`
(`reconcile.go:452`) today hardcodes `StatusRunning` as the precondition it
re-checks inside the `store.Update` transaction before moving a record to
`StatusUpdating`. It becomes `markShielded(ctx, id string, from
store.Status) (bool, error)` — same optimistic-concurrency shape (stands
down silently, `false, nil`, if the record is no longer `from` by the time
the transaction runs), parameterized on the expected starting status.
`wakeAgent`'s own call site becomes `markShielded(ctx, a.ID,
store.StatusRunning)` — a pure rename at that call site, no behavior change.
`StopAgent` calls `markShielded(ctx, id, store.StatusRunning)`; `ResumeAgent`
calls `markShielded(ctx, id, store.StatusStopped)`.

**Shared rebuild helper.** `wakeAgent`'s body from its own `markShielded`
call's success onward — `removeAgentContainer`, `startAgentContainer(...,
firstInvocationArgs(a, sessionResume)...)`, `waitTmuxSession` — is extracted
into `m.rebuildAgentContainer(ctx, a store.Agent, rb resolvedBackend) error`.
It does **not** include `ensureDindRunning`, the agent-container inspect/
early-return-if-already-running check, or `resolveBackendFromAgent`: those
stay in `wakeAgent` exactly where they are today (before its shield call),
since `wakeAgent` deliberately skips the shield and every write that follows
it when nothing actually needs waking — a hot-path optimization for a
function reconcile calls every tick, which this spec must not regress.
`ResumeAgent`, by contrast, already knows from its own precondition that the
store says `StatusStopped`; it always calls `ensureDindRunning`,
`resolveBackendFromAgent`, its own `markShielded(Stopped)`, then
`rebuildAgentContainer`, unconditionally — it has no hot-path reconcile
caller to protect, so there is no cost to always taking the full path, even
in the rare case where the agent container turns out to already be running
underneath a stale `StatusStopped` record (that case is not special-cased;
`rebuildAgentContainer` tears it down and rebuilds it like any other).

**`StopAgent`:**
1. `m.inFlight.tryBegin(id)` / `defer release` (`ErrOperationInFlight` if busy).
2. Load the record; `ErrNotStoppable` unless `StatusRunning`.
3. `markShielded(ctx, id, store.StatusRunning)`.
4. `m.docker.ContainerStop` on the agent container, then on the DinD
   sidecar — both attempted and their errors joined (`errors.Join`), the
   same pattern `removeAgentContainer` already uses, so one container
   failing to stop doesn't leave the other needlessly still running.
5. Any error → `markStopError(ctx, id, err)` (`StatusError`, explanatory
   `ErrorMessage`, container IDs and volumes left untouched — unlike the
   rebuild helper's own failure path, nothing was removed here, so there is
   nothing to clear). Success → `markStopped(ctx, id)` (`StatusStopped`).

**`ResumeAgent`:**
1. `m.inFlight.tryBegin(id)` / `defer release`.
2. Load the record; `ErrNotResumable` unless `StatusStopped`.
3. `ensureDindRunning(ctx, a)`.
4. `resolveBackendFromAgent(ctx, a)`.
5. `markShielded(ctx, id, store.StatusStopped)`.
6. `rebuildAgentContainer(ctx, a, rb)` (the shared helper described above).
7. Any error from steps 3-6 → `markRebuildError(ctx, id, err)` — `wakeAgent`'s
   own `markWakeError` generalized the same way `markShielded` was (message
   text unchanged; it already fit a second caller as-is), reused by both.
   Success → `markRunning(ctx, &a)` (the same call `wakeAgent` ends with).

`wakeAgent`'s own existing tests continue to pin its observable behavior —
including the early-return/no-write path when nothing needs waking —
unchanged across this extraction and the two renames.

**`ApplyOnComplete`'s `"stop"` case** (`internal/agent/schedule.go:111-128`)
gains one line: stop the DinD sidecar container too (decision 2) — the only
behavior change to existing code this spec makes outside the new methods.

## API (`internal/api`)

```
POST /api/agents/{id}/stop    -> h.handleStopAgent
POST /api/agents/{id}/resume  -> h.handleResumeAgent
```

Error mapping, mirroring `handleUpdate`'s own table shape exactly:

| Condition | Status | Code |
|---|---|---|
| `store.IsNotFound` | 404 | `not_found` |
| `agent.IsOperationInFlight` | 409 | `operation_in_flight` (reused, same message Update already uses) |
| `agent.IsNotStoppable` (Stop only) | 409 | `not_stoppable` — "only a running agent can be stopped" |
| `agent.IsNotResumable` (Resume only) | 409 | `not_resumable` — "only a stopped agent can be resumed" |
| anything else | 500 | `internal` |

New constants in `internal/api/errors.go`: `CodeNotStoppable = "not_stoppable"`,
`CodeNotResumable = "not_resumable"`. `CodeOperationInFlight` and
`CodeNotFound` are reused, not duplicated.

Both handlers respond 200 with the updated `agentView`, consistent with
`handleUpdate`/`handleRename`. Both routes are added to the
`methodNotAllowed` catch-all list alongside the other `/api/agents/{id}/...`
routes (`internal/api/handlers.go:256` ff).

## UI (`web/`)

Two new items in `renderAgentDetail`'s existing `⋮` options menu
(`web/terminal.js`), alongside `view-context` / `agent-info` /
`update-agent`:
- `data-action="stop-agent"`, label "Stop agent" — rendered only when the
  fetched agent's `status === "running"` (the same hidden/shown treatment
  `upgradeBtn` already gets off `upgrade_available`/`upgrade_ready`).
- `data-action="resume-agent"`, label "Resume agent" — rendered only when
  `status === "stopped"`.

The two are mutually exclusive single-status gates (unlike Update, which
already permits running/stopped/error) — an agent in any other status
(creating, error, updating, deleting) shows neither item.

Stop is wired like the existing `deleteBtn` handler: a
`window.OperatorConfirm.show(...)` confirmation (non-danger styling per
decision 4) gating a `POST /api/agents/{id}/stop`; on success, re-render the
detail page so the menu's visible items (and any other status-dependent
chrome) refresh against the new state. Resume is the same POST pattern with
no confirmation step.

## Testing

- `internal/agent`: `StopAgent` — happy path (both `ContainerStop` calls
  made, ends `StatusStopped`); rejects a non-Running agent
  (`ErrNotStoppable`); a `ContainerStop` failure lands `StatusError` with
  container IDs/volumes intact; a concurrent in-flight operation 409s via
  `ErrOperationInFlight`. `ResumeAgent` — happy path (`ensureDindRunning` +
  remove/recreate-with-resume-args called, ends `StatusRunning`); rejects a
  non-Stopped agent (`ErrNotResumable`); a rebuild failure lands
  `StatusError`, same shape as an unsuccessful `wakeAgent`; a concurrent
  in-flight operation 409s. `wakeAgent`'s own existing test suite continues
  to pass unchanged against the extracted `rebuildAgentContainer` helper.
- `internal/agent`: the existing `ApplyOnComplete`/`"stop"` test gains an
  assertion that the DinD sidecar's `ContainerStop` was called too.
- `internal/api`: handler tests for both new routes covering the full
  error-mapping table (404/409/409/200), mirroring `handleUpdate`'s own test
  shape.
- `web`: the detail menu's Stop item renders only for a Running agent, the
  Resume item only for a Stopped one; Stop's click path goes through
  `OperatorConfirm` before the POST and does **not** POST when declined;
  Resume's click path POSTs with no confirm step.
- **Review-focus items pinned explicitly** (failure modes the spec implies
  but no happy-path test would catch): a partial Stop/Resume failure lands
  `StatusError` rather than staying stuck in `StatusUpdating`; a second
  Stop/Resume request for the same agent while one is already in flight
  gets a clean 409 instead of racing Docker calls against each other;
  `ResumeAgent` succeeds with no `Template` lookup at all — `store.Agent`
  alone already carries everything needed to recreate the container,
  independent of whether the originating Template still exists.
