# Design: separate CronAgent from Template

**Status:** design, not yet implemented.

**Branch:** `feat/cron-agents-design`, branched from `main` (which already
has #233's scheduled-agents feature merged).

**Issue:** none filed — a direct follow-up request on the just-shipped
scheduled-agents feature.

## Why

#233 shipped scheduling by adding `Schedule`/`Enabled`/`Task`/`OnComplete`
directly onto `store.Template`. That conflates two different concepts: a
*Template* is a reusable bundle of agent-creation config (backend, model,
repo, image tag, harness — "how to build this kind of agent"), while a
*recurring job* is "run this specific task, on this schedule, cleaning up
this way." Under the current design, reusing one Template's config across
two differently-scheduled jobs is impossible without duplicating the whole
Template — the schedule, task, and cleanup policy are stuck inside it.

This spec splits that into two entities: `Template` reverts to pure
creation config (exactly its pre-#233 shape, plus `Harness`, which stays —
see decision 2), and a new `CronAgent` entity owns the recurring-job
definition, referencing a `Template` by ID. A `CronAgent` is the thing a
human creates to say "run this template's config on this schedule, with
this task, cleaned up this way" — the same Template can back any number of
differently-scheduled `CronAgent`s.

## Decisions locked during brainstorming

1. **`Task` and `OnComplete` move to `CronAgent`.** Both are properties of
   the recurring job, not the reusable config — the stated motivation (reuse
   one Template across multiple schedules) requires this. `Enabled` moves
   too, same reasoning.
2. **`Harness` stays on `Template`.** It is creation config (which CLI/image
   the agent runs), confirmed already the right home for it when it was
   added in #233 for the same reason a cron trigger has no human present to
   pick it. `CronAgent` inherits it transitively through the `Template` it
   references; it is not duplicated onto `CronAgent`.
3. **No data migration.** #233 only just merged; no real `CronAgent`-worthy
   templates exist in production yet. `Template.Schedule`/`Enabled`/`Task`/
   `OnComplete` and `ScheduleRun.TemplateID` are removed/renamed outright,
   not migrated.
4. **`CronAgent.Name` becomes the created agent's `store.Agent.Name` too,**
   not just a label in the Cron Agents list. Today a template-created agent
   has no name at all (`Template.Name` is deliberately never copied onto the
   agent it creates, per #233's own `toAgentFieldsRequest` exclusion) — this
   closes that gap specifically for scheduled agents, which otherwise show
   up with nothing but an ID.
5. **Deleting a `Template` that still has any `CronAgent` referencing it is
   rejected (409).** The alternative — allow it, let the `CronAgent` fail at
   its next trigger — was considered and declined: a `CronAgent` silently
   going stale because its config disappeared out from under it is worse
   than an explicit, immediate block the user can act on.
6. **Naming: `CronAgent`, matching the user's own term.** Store type
   `store.CronAgent`, API routes under `/api/cron-agents`, sidebar button
   and page titled "Cron Agents."

## Data model

```go
// internal/store/template.go — Template reverts to its pre-#233 shape
// plus Harness, which #233 added and this spec keeps (decision 2).
type Template struct {
    ID, Name, Description string
    Harness string // config.HarnessClaudeCode or config.HarnessOpenCode
    Backend, Model, FastModel, OllamaURL, Repo string
    AutoCompactThreshold, MaxContextTokens, ImageTag, AutoMode string
    CreatedAt, UpdatedAt time.Time
    // Schedule, Enabled, Task, OnComplete -- REMOVED, moved to CronAgent.
}
```

```go
// internal/store/cronagent.go — new record, new bucket, CRUD mirroring
// Template's own shape exactly (CreateCronAgent/GetCronAgent/
// ListCronAgents/UpdateCronAgent/DeleteCronAgent).
type CronAgent struct {
    ID         string
    // Name identifies this CronAgent in the Cron Agents list AND becomes
    // store.Agent.Name on every agent CreateFromCronAgent creates.
    Name       string
    // TemplateID references the Template whose config (backend, model,
    // harness, ...) this job's agents are created from. Required; DeleteTemplate
    // refuses (409) while any CronAgent still references it (decision 5).
    TemplateID string
    Schedule   string // cron expression, same agent.ParseSchedule as #233
    Enabled    bool
    Task       string // the one-shot prompt
    OnComplete string // "stop" | "delete" | "leave"
    CreatedAt, UpdatedAt time.Time
}
```

`store.ScheduleRun.TemplateID` is renamed to `CronAgentID` — a run belongs
to the CronAgent that triggered it, not directly to a Template. `AgentID`
is unchanged. `(*Store) ListScheduleRunsByTemplate` is renamed
`ListScheduleRunsByCronAgent` to match.

`internal/agent/templates.go` gains `Manager`-level pass-throughs for
`CronAgent`'s CRUD (`CreateCronAgent`, `GetCronAgent`, `ListCronAgents`,
`UpdateCronAgent`, `DeleteCronAgent`), mirroring `Template`'s own
pass-throughs exactly — `internal/api` depends on these through the
`AgentManager` interface the same way it already depends on the Template
ones, and the `cmd/docker-operator` ticker's `schedulingDeps` interface
needs `ListScheduleRunsByCronAgent`/`CreateScheduleRun`/`FinishScheduleRun`/
`ListScheduleRunsByStatus`/`GetCronAgent` on `Manager` before the ticker
task runs — the exact ordering lesson #233's own pre-flight review learned
the hard way (and initially got only half-right), worth stating explicitly
here so the implementation plan gets it right the first time: add every
`CronAgent`-related `Manager` pass-through in the SAME task that adds
`CreateFromCronAgent`/`CheckScheduledRunStatus`/`ApplyOnComplete`, not
split across tasks by which package happens to call it first.

## Validation and referential integrity

`validateTemplateFields` (`internal/api/templates.go`) reverts to exactly
its pre-#233 shape: no `Schedule`/`Task`/`OnComplete` checks, no
opencode-with-schedule rejection. It still validates `Harness` the way #233
left it (decision 2) — just with nothing to cross-check against anymore.

A new `validateCronAgentFields`: `Name` and `TemplateID` required;
`TemplateID` must reference an existing `Template` (a lookup, not just a
non-empty check); `Schedule` required, parsed with the same
`agent.ParseSchedule` the ticker uses; `Task` required; `OnComplete` one of
`"stop"`/`"delete"`/`"leave"`; and the opencode rejection moves here too,
now checking the *referenced Template's* `Harness` — the ticker's own
defensive check (below) is still insurance for a record that reached this
state before validation existed, same role it played in #233.

`DeleteTemplate` (`internal/store/template.go`) gains a precondition: if
any `CronAgent` has `TemplateID` equal to the one being deleted, return a
new `ErrTemplateInUse` (mapped to 409 by `internal/api`), naming at least
one referencing `CronAgent` in the error so the user knows what to fix
first (decision 5).

## The ticker

`cmd/docker-operator/schedule.go`'s `fireDueTemplates` becomes
`fireDueCronAgents`: iterates `CronAgent`s with `Enabled == true` (not
Templates with a non-empty `Schedule`), computes `TemplateDue` over the
`CronAgent`'s own `CreatedAt`/`UpdatedAt`/most-recent-`ScheduleRun.StartedAt`
(same anchor logic as #233, keyed by `CronAgentID` now). For each due
`CronAgent`: load its `Template` (`GetTemplate`); the opencode-defensive
check now inspects *that* Template's `Harness`; build the `CreateRequest`
from the Template's fields plus `Name: ca.Name` and `OneShotTask: ca.Task`;
record a `ScheduleRun` keyed by `CronAgentID` exactly as #233 already does
by `TemplateID` (including the create-failure ordering fix: always record
a run, finalized as error on a `Create` failure).

`checkRunningSchedules` is unchanged in shape — it already operates on
`ScheduleRun.AgentID`, never needed the Template directly, except where it
loads the owning record to read `OnComplete` for `ApplyOnComplete`: that
load becomes `GetCronAgent(run.CronAgentID)` instead of `GetTemplate`,
including the existing "owning record deleted mid-flight" fallback (#233's
fix 7's reasoning carries over unchanged: a `CronAgent` deleted while its
run was in flight still finalizes the run, falling back to the zero-value
`OnComplete` → "leave").

`Manager.CreateFromTemplate` (`internal/agent/schedule.go`) is renamed
`CreateFromCronAgent(ctx, ca store.CronAgent) (store.Agent, error)`: resolves
`ca.TemplateID` to a `Template` internally, builds the `CreateRequest` from
it, and sets `Name: ca.Name`, `OneShotTask: ca.Task` — the two fields that
now come from the `CronAgent`, not the `Template`. `ApplyOnComplete`'s
signature changes from `(ctx, tmpl store.Template, run store.ScheduleRun)`
to `(ctx, ca store.CronAgent, run store.ScheduleRun)`; its body is otherwise
unchanged (it only ever read `.OnComplete`).

## UI

The **Templates** page reverts to exactly its pre-#233 shape (keeping the
already-shipped styling fix: sectioned fields, the collapsed Advanced
disclosure) — minus the whole "Scheduling" section and the run-history list,
both of which move to the new page below.

A new **Cron Agents** page (new sidebar button, same
`teardownActiveView()` → `renderSidebar()` → `renderXPage(mainArea)` pattern
every other page already uses): lists every `CronAgent` (name, schedule
summary, enabled/paused, which Template it uses), with create/edit/delete.
The edit form: `Name`, a `<select>` of existing Templates (populated from
`GET /api/templates`, the same way the create-agent form's own template
picker already works), `Schedule`, `Enabled`, `Task`, `OnComplete` — and the
run-history list below it, reusing #233's `renderScheduleRunList` unchanged
except for which endpoint it reads from.

New routes: full CRUD under `/api/cron-agents` (mirroring `/api/templates`'s
own shape exactly) and `GET /api/cron-agents/{id}/runs` (replacing
`/api/templates/{id}/runs`, which is removed).

## Testing

- `internal/store`: `CronAgent` CRUD (new bucket, mirrors `Template`'s own
  test shape); `Template`'s four scheduling fields are gone — a pre-existing
  record that still has them in its raw JSON must decode cleanly with them
  simply ignored (the mirror image of #233's "pre-schedule record decodes
  inert" test); `DeleteTemplate` returns `ErrTemplateInUse` (409-mappable)
  when a `CronAgent` references it, and succeeds once the reference is gone.
- `internal/agent`: `CreateFromCronAgent` sets `Name`/`OneShotTask` from the
  `CronAgent`, config from the resolved `Template`; `ApplyOnComplete`'s new
  signature; every completion-status test from #233 (`"running"`/`"done"`/
  `"failed"`/`"gone"`) still passes unchanged, since `CheckScheduledRunStatus`
  never depended on Template vs. CronAgent in the first place.
- `cmd/docker-operator`: `fireDueCronAgents` fires due, enabled CronAgents
  (not Templates); the opencode-defensive check against the *referenced*
  Template's Harness; the create-failure-records-an-error-run fix, the
  gone-agent-finalizes-as-error fix, and the multiple-due-in-one-tick
  guarantee — all re-pinned against CronAgent instead of Template, since the
  entity the ticker iterates changed even though the guarantees didn't.
- `internal/api`: `validateTemplateFields` has no scheduling-related tests
  left (they move); new `validateCronAgentFields` tests mirror #233's own
  (schedule-requires-the-rest-together, malformed cron rejected, opencode
  rejected — now via the referenced Template) plus a new one for
  `TemplateID` referencing a nonexistent Template; `DeleteTemplate`'s 409
  surfaces correctly through the handler.
- Web UI: `renderTemplateForm`'s scheduling section and the Advanced/sections
  split from the styling fix stay, scheduling fields are gone; a new
  `renderCronAgentForm`/`renderCronAgentsPage` pair, render-tested the same
  way `renderTemplateForm`/`renderTemplatesPage` already are; `cronAgents.js`
  mirrors `templates.js`'s shape.
