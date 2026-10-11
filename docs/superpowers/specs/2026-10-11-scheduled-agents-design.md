# Design: scheduled (cron-style) agents

**Status:** design, not yet implemented.

**Branch:** not yet created — this spec is written against `main`.

**Issue:** [#228](https://github.com/psenna/ai-sandbox/issues/228), "Scheduled /
recurring agents (cron-style triggers)."

## Why

Today every agent is created explicitly, by a human, through the web UI or
API, and runs until deleted. There is no scheduling primitive anywhere in
`internal/agent` or `cmd/docker-operator` — a routine, repeatable task (a
daily dependency-update check, a recurring report, a periodic cleanup job)
requires a human to re-trigger it, or an agent left running indefinitely for
something that only needs to run occasionally.

Issue #228 deliberately filed this without a design, flagging it as "the
biggest-scope idea of the three" needing real decisions first. This spec
makes those decisions.

## Decisions locked during brainstorming

1. **Config lives on `store.Template`, extended, not a new concept.**
   Templates already bundle "what to create" (backend, model, repo, image
   tag, auto-mode); a recurring job is the same kind of thing plus a
   schedule. Templates keep their existing name and existing meaning for
   every template that has no schedule — this is a pure extension, not a
   breaking change to the current save-current-form-as-template flow.
2. **A fresh agent every trigger, never a reused one.** No "is the previous
   run still going" detection, no session-continuity questions. Each
   scheduled run is an independent agent, created and (per decision 5)
   cleaned up on its own. Confirmed against the existing `wakeAgent`
   mechanism: it exists to *resume a previous conversation*
   (`--continue`/`--resume`), not to hand a running or dormant agent a brand
   new task — reusing it for scheduling would need new busy-detection and
   task-injection machinery that does not exist today, for no benefit this
   feature needs.
3. **The task itself is new text on `Template`, not inferred from the
   repo/CLAUDE.md alone.** Confirmed neither `store.Template` nor
   `CreateRequest` carries anything resembling "what to tell the agent to
   do" today — an agent's work comes entirely from a human typing into its
   terminal after creation. A scheduled run needs an explicit instruction.
4. **Completion is detected via Claude Code's one-shot mode and a free
   signal `claude-supervisor.sh` already gives us.** A scheduled run invokes
   `claude -p "<task>"` instead of starting an interactive session.
   `claude-supervisor.sh` (confirmed, read in full) already treats a clean
   (status 0) exit specially: it does **not** restart the process, and logs
   "claude exited 0 (deliberate); not restarting" — exactly what `-p`
   finishing produces. The only gap is distinguishing that clean finish from
   the *other* way the supervisor stops restarting (five crash-loop restarts
   in 60s, which also exits non-zero and leaves a dead pane) — closed by one
   small, additive change to that shared script: write a sentinel file,
   `/workspace/.task-complete` (same directory and naming convention as
   `wsbridge.OutputLogPath`'s `/workspace/.agent-output.log`), right after
   the clean-exit log line. Every agent's script gains this, not just
   scheduled ones; it is inert (never read) unless something is polling for
   it.
5. **Scheduling targets Claude Code only in v1.** opencode's equivalent
   one-shot completion behavior has not been verified. `Template.Harness`
   is added regardless (see decision 7), but a `Template` whose `Harness` is
   `config.HarnessOpenCode` is rejected when it also carries a `Schedule` —
   a validation rule, not a silent restriction. Extending to opencode is a
   self-contained follow-up once its one-shot story is confirmed.
6. **Per-template completion policy: stop, delete, or leave.** A new
   `OnComplete` field (`"stop"` | `"delete"` | `"leave"`) lets a noisy daily
   check clean itself up while a report someone wants to review stops but
   stays inspectable. `"delete"` composes for free with the already-shipped
   transcript archive (PR #230): `Delete`'s `teardown` archives the session
   into `transcripts/<id>/` as its first step, before anything is torn down,
   so the content survives regardless of which policy a template picks.
7. **`Template` gains a required `Harness` field.** Confirmed `store.Template`
   has no `Harness` field today — a human always picks it separately in the
   create form, even after applying a template. A cron trigger has no human
   present to make that choice, so this is needed regardless of the harness-
   scope decision above.
8. **Real cron syntax, not a constrained interval model.** Matches the
   issue's own title and ordinary cron-literate expectations (weekdays,
   monthly, etc.), at the cost of one new dependency
   (`github.com/robfig/cron/v3` — stdlib-only transitively, used only for its
   parser and `Schedule.Next`, never its own scheduling runtime; this
   project drives its own ticker, the same way `startPeriodicReconcile`
   does).
9. **A schedule is pausable without losing its configuration.** A new
   `Enabled` bool on `Template`; the trigger ticker skips a disabled
   schedule's `Next()` check entirely. Lets a seasonal or noisy job's full
   config sit idle without deleting and later re-typing it.
10. **A durable run-history record, independent of the agent's own
    lifecycle.** A new `ScheduleRun` store record
    (`ID, TemplateID, AgentID, StartedAt, FinishedAt *time.Time, Status`)
    survives the agent being deleted (`OnComplete: delete`), unlike querying
    live `store.Agent` records, which disappear. This is also the single
    source of truth for "when did this template last fire," rather than a
    second, duplicate timestamp field on `Template` itself.
11. **A new standalone Template page replaces the inline template bar.**
    Confirmed there is no dedicated template UI today: templates live in a
    small bar on the create-agent form (a picker, a "save current form as
    template" button, a delete button) — no schema `Harness`, `Schedule`,
    `Task`, or `OnComplete` fit that model, since they aren't create-form
    fields. The new page (a sidebar entry, following the Activity/Files/
    Settings pattern) becomes the one place a template's full definition —
    old fields and new — is created, edited, and deleted, and where a
    scheduled template's run history is shown. The create form's bar shrinks
    to a pure picker: apply a template to prefill the form, nothing else.

## Data model

```go
// internal/store/template.go — new fields on Template (and
// TemplateCreateSpec, mirrored exactly, as every existing field already is)
type Template struct {
    // ... existing fields unchanged ...

    // Harness is the CLI harness a scheduled run uses: config.HarnessClaudeCode
    // or config.HarnessOpenCode. Required (unlike today, where a template
    // carries no harness at all) because a cron trigger has no human present
    // to choose one.
    Harness string `json:"harness"`

    // Schedule is a standard 5-field cron expression ("0 9 * * *"). Empty
    // means this template is NOT a scheduled job -- it behaves exactly as
    // every template does today, a pure create-form prefill.
    Schedule string `json:"schedule,omitempty"`
    // Enabled gates whether a non-empty Schedule actually fires. Ignored
    // when Schedule is empty.
    Enabled bool `json:"enabled,omitempty"`
    // Task is the prompt text passed to the harness's one-shot invocation
    // ("claude -p <Task>") on each triggered run. Required when Schedule is
    // non-empty; ignored otherwise.
    Task string `json:"task,omitempty"`
    // OnComplete decides what happens to a scheduled run's agent once its
    // task finishes: "stop" (ContainerStop, left inspectable), "delete"
    // (Delete, archiving the transcript first), or "leave" (do nothing).
    // Required when Schedule is non-empty; ignored otherwise.
    OnComplete string `json:"on_complete,omitempty"`
}
```

```go
// internal/store/schedulerun.go — new record, new bucket, same CRUD shape
// as Template's own (Create/Get/List; no Update -- only two transitions,
// "mark running" at creation and "finalize" at completion, both simple
// enough for direct store calls with no mutator-function indirection).
type ScheduleRun struct {
    ID          string     `json:"id"`
    TemplateID  string     `json:"template_id"`
    AgentID     string     `json:"agent_id"`
    StartedAt   time.Time  `json:"started_at"`
    FinishedAt  *time.Time `json:"finished_at,omitempty"`
    // Status: "running" | "success" | "error".
    Status string `json:"status"`
}
```

`CreateRequest` (and the internal translation Create already does from a
template's fields today) gains one new optional field, a one-shot task
string. When set, the agent's first invocation runs `claude -p "<task>"`
instead of starting an interactive session — a new branch alongside
`firstInvocationArgs`'s existing fresh/continue/resume cases, since a
one-shot run is not a session-resumption concern at all. Exact argument
construction (how it composes with `autoModeArgs`, escaping) is a planning-
stage detail, not a design-stage one.

## Trigger and completion mechanism

One new ticker in `cmd/docker-operator`, modeled directly on
`startPeriodicReconcile` (cancel-based stop function, overlap-guarded,
ticking roughly every minute — cron's own granularity). Each tick does two
things, in order:

**1. Fire due schedules.** For every `Template` with `Enabled == true` and a
non-empty `Schedule`: look up its most recent `ScheduleRun` (by `TemplateID`,
most recent `StartedAt`), or fall back to `Template.CreatedAt` if it has
never run. Parse `Schedule` with `cron.ParseStandard` and call
`Schedule.Next(anchor)`; if that next time is `<= now`, the schedule is due.
For each due template:
- `internal/api`'s create/update validation (see Testing) is the real
  enforcement of decision 5, rejecting `Harness == config.HarnessOpenCode`
  with a non-empty `Schedule` before such a template can ever be saved; the
  ticker's own check here is defensive only, for a record saved before that
  validation existed or reached inconsistently some other way (log and
  skip, like any other best-effort periodic pass's single-item failure).
- Create a `ScheduleRun` (`Status: "running"`, `StartedAt: now`).
- Call `Manager.Create` with a `CreateRequest` built from the template's
  fields, plus the new one-shot task field set from `Template.Task`. This
  translation is NEW: confirmed the only existing `Template` →
  create-agent-fields mapping is `web/app.js`'s `applyTemplateToForm`, pure
  client-side JS that prefills the create form for a human to then submit —
  there is no server-side equivalent today, since nothing before this
  feature ever created an agent from a template without a human in the
  loop.
- Stamp the new agent's ID onto the `ScheduleRun`.

A `Manager.Create` failure here is logged and the `ScheduleRun` is finalized
as `Status: "error"` immediately (no agent was ever created to track) —
mirroring how every other best-effort periodic pass in this codebase treats
a single failure as non-fatal to the rest of the tick.

**2. Check in-flight runs for completion.** For every `ScheduleRun` with
`Status == "running"`: exec a check for the completion sentinel file inside
the agent's container, the same one-shot-exec-and-read-exit-code mechanism
`waitTmuxSession` already uses (bounded by the existing `ExecTimeout`). If
present:
- Finalize the `ScheduleRun` (`Status: "success"`, `FinishedAt: now`) — a
  future refinement could distinguish a non-zero `claude -p` exit as
  `"error"` by also having the sentinel record the exit code, but a binary
  success/still-running signal is enough for v1.
- Apply `Template.OnComplete`: `"stop"` calls the existing `ContainerStop`
  path, `"delete"` calls the existing `Delete` (which archives the
  transcript first, per PR #230, regardless of this feature), `"leave"` does
  nothing further.

A container whose agent record disappeared out from under this check (a
human manually deleted it mid-run) is treated like any other
already-gone-is-success case elsewhere in this codebase: the `ScheduleRun`
is finalized as `"error"` with that reason, not retried.

## UI

A new sidebar entry, **Templates**, alongside Activity/Files/Settings,
following the same `teardownActiveView()` → clear selection → `renderSidebar()`
→ a new `renderTemplates(mainArea)` pattern every other main-area view
already uses. It lists every template (name, and — when scheduled — a
human-readable schedule summary and enabled/disabled state), with
create/edit/delete actions. The edit form covers every field: the 9
existing agent-config fields, plus `Harness`, `Schedule`, `Enabled`, `Task`,
and `OnComplete`.

For a template with a schedule, the same edit view shows its run history:
the most recent `ScheduleRun`s (newest first), each with status, started/
finished timestamps, and a link — to the live agent if `OnComplete` left it
around (`"stop"` or `"leave"`), or straight to its archived transcript under
`transcripts/<id>/` (via the existing, fully generic Files browser) if it was
deleted.

The create-agent form's existing template bar shrinks to a pure picker:
select a template, apply it to prefill the form. The current "save current
form as template" and "delete template" controls move to the new page,
since a template is no longer fully describable by what the create form
itself captures.

## Testing

- `internal/store`: `ScheduleRun` CRUD (new bucket, mirroring `Template`'s
  own test shape); `Template`'s four new fields round-trip through
  Create/Update/Get exactly like every existing field.
- `internal/agent`: the one-shot `CreateRequest` field produces the right
  harness args (mocked at the `firstInvocationArgs`-equivalent layer, not a
  real container); a scheduled-trigger helper that, given a set of
  `Template`s and `ScheduleRun` history, decides which are due (pure
  function over fakes/fixtures, no ticker involved) — the same style
  `TestStartPeriodicReconcile_OverlapSkipped` uses to test `main.go`'s
  ticker shape without a real timer; completion-check logic against the
  fake docker client's exec-call recording.
- `cmd/docker-operator`: the new ticker's overlap-guard and stop-function
  shape, mirroring `main_test.go`'s existing coverage of
  `startPeriodicReconcile`.
- `internal/api`: template CRUD's validation of the four new fields
  (`Harness` required; `Schedule`/`Task`/`OnComplete` required together,
  empty together; `Schedule` parses as valid cron; `OnComplete` one of the
  three known values; `Harness == opencode` rejected when `Schedule` is set).
- `agent/claude-supervisor.sh`: a script-level check (this repo's existing
  shell-script testing convention, if any, or a documented manual check)
  that the sentinel file appears only after a clean (status 0) exit, never
  after the crash-loop give-up path.
- Web UI (`web/*.test.js`): `renderTemplates`' markup for the list, the edit
  form's new fields, and the run-history section; the shrunk template bar's
  picker-only markup.
- No new test for opencode scheduling: decision 5 makes it a rejected
  validation case, not a code path that runs.
