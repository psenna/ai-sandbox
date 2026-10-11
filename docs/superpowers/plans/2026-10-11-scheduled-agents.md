# Scheduled Agents Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a `store.Template` define a recurring job — a cron schedule,
a one-shot task, and a completion policy — so docker-operator creates,
runs, and cleans up agents automatically, with a durable per-schedule run
history and a dedicated Templates page in the web UI.

**Architecture:** `Template` is extended in place (no new top-level
concept) with `Harness`/`Schedule`/`Enabled`/`Task`/`OnComplete`. A new
`ScheduleRun` store record tracks one run per trigger, independent of the
agent's own lifecycle. One new ticker in `cmd/docker-operator`, modeled on
the existing `startPeriodicReconcile`, does two things each tick: fires any
due, enabled schedule (creating a fresh agent via a new Template→
`CreateRequest` translation, running the task through Claude Code's
one-shot `-p` mode) and checks every in-flight run for completion (a tiny,
additive sentinel file in `claude-supervisor.sh` gives a free, unambiguous
"this task finished cleanly" signal). A new Templates page in the web UI
replaces the inline template bar as the one place a template's full
definition — old fields and new — is created, edited, and deleted.

**Tech Stack:** Go 1.26, `go.etcd.io/bbolt` (existing store), one new
dependency `github.com/robfig/cron/v3` (cron parsing only, never its own
scheduler), the existing `ExecClient`/`dockerclienttest.Fake` test harness,
vanilla JS (`web/*.js`, same house pattern as `files.js`/`render.js`).

**Spec:** `docs/superpowers/specs/2026-10-11-scheduled-agents-design.md`

## Global Constraints

- Go 1.26.9 (this module's `go.mod`).
- New dependency `github.com/robfig/cron/v3`, installed via DependaProxy's
  goproxy (`GOPROXY=http://dependaproxy:8080/goproxy`) — used ONLY for
  `cron.ParseStandard` and the returned `cron.Schedule`'s `Next` method,
  never its own `cron.Cron` scheduling runtime; this project drives its own
  ticker.
- Scheduling targets Claude Code only in v1 — a `Template` with
  `Harness == config.HarnessOpenCode` and a non-empty `Schedule` is a
  rejected (400) combination at the API layer, not a code path that ever
  runs.
- A scheduled run is ALWAYS a fresh agent; reusing an existing one is out
  of scope (spec decision 2).
- The `claude-supervisor.sh` sentinel-file addition applies to EVERY
  agent's script, not just scheduled ones, and must stay inert (never read
  by anything) for a non-scheduled agent.
- `transcripts/<id>/` archiving (already shipped, PR #230) is reused
  as-is for `OnComplete: "delete"` — no changes to `internal/agent/transcript.go`.
- Any edit under `docker-operator/web/` requires `make sync-web-embed`
  (or at least `make web-embed-check`) before the branch is considered
  done, per `docker-operator/CLAUDE.md`'s committed-copy rule.

## Review Focus

- **A `Template` saved before these fields existed.** Its JSON has no
  `schedule`/`task`/`on_complete`/`harness` keys at all; decoding must
  zero-value them (`Schedule: ""`, `Enabled: false`), which the "empty
  `Schedule` means not scheduled" rule already treats as fully inert —
  covered in Task 1.
- **Deleting a `Template` that still has `ScheduleRun` history, or an
  in-flight run.** `DeleteTemplate` must not cascade-delete
  `ScheduleRun`s (the history is meant to outlive the template, the same
  way a transcript outlives its agent) and a run whose template
  disappeared mid-flight must still finish normally — its completion
  check only needs the `AgentID`, never the template — covered in Task 1
  (the cascade check) and Task 3 (the in-flight-run check).
- **Two (or more) schedules due in the same tick** (e.g. the operator was
  down for hours and several `Next()` times are all now in the past).
  The ticker must fire every due schedule in that one tick, and one
  template's `Manager.Create` failure must not block the others —
  covered in Task 3.
- **A `ScheduleRun` whose agent is gone** (deleted out-of-band by a human,
  or already removed by a prior `OnComplete: "delete"` pass that crashed
  before finalizing the run). The completion check must finalize it as
  `"error"`, not retry forever or crash the tick — covered in Task 2 and
  Task 3.
- **A malformed or non-standard cron string** (6 fields with seconds, a
  typo, an empty string submitted as non-empty-looking whitespace). API
  validation must reject it (400) before it ever reaches the ticker,
  where an unparseable `Schedule` would have nothing sane to call
  `Next()` on — covered in Task 4.

---

## Task 1: `internal/store` — Template fields and the `ScheduleRun` record

**Files:**
- Modify: `internal/store/template.go` (add `Harness`, `Schedule`,
  `Enabled`, `Task`, `OnComplete` to `Template` and
  `TemplateCreateSpec`, ~line 69 and ~line 112)
- Modify: `internal/agent/templates.go` (extend `UpdateTemplate`'s field
  copy, ~line 44)
- Create: `internal/store/schedulerun.go`
- Create: `internal/store/schedulerun_test.go`
- Modify: `internal/store/store.go` (register the new bucket, ~line 413)

**Interfaces:**
- Produces: `store.Template.Harness/Schedule/Enabled/Task/OnComplete`
  (all strings except `Enabled bool`); `store.ScheduleRun{ID, TemplateID,
  AgentID, StartedAt, FinishedAt *time.Time, Status}`;
  `store.ScheduleRunStatusRunning/Success/Error` (the three valid
  `Status` values, as string constants); `store.NewScheduleRunID() (string,
  error)`; `(*Store) CreateScheduleRun(ctx, templateID, agentID string)
  (ScheduleRun, error)`; `(*Store) FinishScheduleRun(ctx, id, status string)
  (ScheduleRun, error)`; `(*Store) GetScheduleRun(ctx, id string)
  (ScheduleRun, error)`; `(*Store) ListScheduleRunsByTemplate(ctx,
  templateID string) ([]ScheduleRun, error)` (newest `StartedAt` first);
  `(*Store) ListScheduleRunsByStatus(ctx, status string) ([]ScheduleRun,
  error)` (any order).
- Consumes: nothing new — mirrors `Template`'s own existing CRUD shape
  exactly (`viewTemplates`/`updateTemplates`/`templatesBucket`/
  `putTemplate`/`decodeTemplate`, all in `internal/store/template.go`).

- [ ] **Step 1: Write the failing tests**

Add to `internal/store/template.go`'s existing test file,
`internal/store/template_test.go` (a new test function; the file already
exists per the current `Template` test coverage):

```go
func TestTemplate_ScheduleFieldsRoundTrip(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	tmpl, err := st.CreateTemplate(ctx, TemplateCreateSpec{
		ID:         "tpl_sched1",
		Name:       "daily report",
		Harness:    "claude-code",
		Schedule:   "0 9 * * *",
		Enabled:    true,
		Task:       "write today's report",
		OnComplete: "delete",
	})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if tmpl.Harness != "claude-code" || tmpl.Schedule != "0 9 * * *" || !tmpl.Enabled ||
		tmpl.Task != "write today's report" || tmpl.OnComplete != "delete" {
		t.Errorf("Template = %+v, want the five new fields set verbatim", tmpl)
	}

	got, err := st.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if got != tmpl {
		t.Errorf("GetTemplate = %+v, want %+v", got, tmpl)
	}
}

// TestTemplate_PreScheduleRecordDecodesInert pins the Review Focus item: a
// template saved before these fields existed (no schedule/task/on_complete/
// harness keys in its JSON at all) must decode with them all zero-valued,
// which this feature's "empty Schedule means not scheduled" rule already
// treats as fully inert.
func TestTemplate_PreScheduleRecordDecodesInert(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	err := st.updateTemplates(ctx, func(b *bbolt.Bucket) error {
		raw := []byte(`{"id":"tpl_old1","name":"old template","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)
		return b.Put([]byte("tpl_old1"), raw)
	})
	if err != nil {
		t.Fatalf("seeding a pre-schedule record: %v", err)
	}

	got, err := st.GetTemplate(ctx, "tpl_old1")
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if got.Harness != "" || got.Schedule != "" || got.Enabled || got.Task != "" || got.OnComplete != "" {
		t.Errorf("pre-schedule record decoded as %+v, want every new field zero-valued", got)
	}
}
```

Create `internal/store/schedulerun_test.go`:

```go
package store

import (
	"context"
	"testing"
)

func TestScheduleRun_CreateAndFinish(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	run, err := st.CreateScheduleRun(ctx, "tpl_1", "agt_1")
	if err != nil {
		t.Fatalf("CreateScheduleRun: %v", err)
	}
	if run.TemplateID != "tpl_1" || run.AgentID != "agt_1" || run.Status != ScheduleRunStatusRunning {
		t.Errorf("ScheduleRun = %+v, want TemplateID/AgentID set and Status running", run)
	}
	if run.StartedAt.IsZero() {
		t.Error("StartedAt is zero, want the time CreateScheduleRun ran")
	}
	if run.FinishedAt != nil {
		t.Errorf("FinishedAt = %v, want nil before finishing", run.FinishedAt)
	}

	finished, err := st.FinishScheduleRun(ctx, run.ID, ScheduleRunStatusSuccess)
	if err != nil {
		t.Fatalf("FinishScheduleRun: %v", err)
	}
	if finished.Status != ScheduleRunStatusSuccess {
		t.Errorf("Status = %q, want %q", finished.Status, ScheduleRunStatusSuccess)
	}
	if finished.FinishedAt == nil || finished.FinishedAt.IsZero() {
		t.Error("FinishedAt not set after FinishScheduleRun")
	}

	got, err := st.GetScheduleRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetScheduleRun: %v", err)
	}
	if got != finished {
		t.Errorf("GetScheduleRun = %+v, want %+v", got, finished)
	}
}

func TestScheduleRun_FinishMissingIsNotFound(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	_, err := st.FinishScheduleRun(ctx, "run_missing", ScheduleRunStatusError)
	if !IsScheduleRunNotFound(err) {
		t.Errorf("FinishScheduleRun(missing) = %v, want IsScheduleRunNotFound", err)
	}
}

// TestScheduleRun_ListByTemplateNewestFirst backs the UI's run-history list
// and the ticker's "anchor on the most recent run" lookup.
func TestScheduleRun_ListByTemplateNewestFirst(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	first, err := st.CreateScheduleRun(ctx, "tpl_a", "agt_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateScheduleRun(ctx, "tpl_b", "agt_2"); err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateScheduleRun(ctx, "tpl_a", "agt_3")
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.ListScheduleRunsByTemplate(ctx, "tpl_a")
	if err != nil {
		t.Fatalf("ListScheduleRunsByTemplate: %v", err)
	}
	if len(got) != 2 || got[0].ID != second.ID || got[1].ID != first.ID {
		t.Errorf("ListScheduleRunsByTemplate(tpl_a) = %+v, want [second, first] newest-first", got)
	}
}

func TestScheduleRun_ListByStatus(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	running, err := st.CreateScheduleRun(ctx, "tpl_a", "agt_1")
	if err != nil {
		t.Fatal(err)
	}
	finishedOne, err := st.CreateScheduleRun(ctx, "tpl_a", "agt_2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishScheduleRun(ctx, finishedOne.ID, ScheduleRunStatusSuccess); err != nil {
		t.Fatal(err)
	}

	got, err := st.ListScheduleRunsByStatus(ctx, ScheduleRunStatusRunning)
	if err != nil {
		t.Fatalf("ListScheduleRunsByStatus: %v", err)
	}
	if len(got) != 1 || got[0].ID != running.ID {
		t.Errorf("ListScheduleRunsByStatus(running) = %+v, want only %q", got, running.ID)
	}
}

// TestDeleteTemplate_DoesNotCascadeScheduleRuns pins the Review Focus
// item: a template's run history is meant to outlive the template itself,
// the same way a transcript outlives its agent -- DeleteTemplate must not
// take ScheduleRun records down with it.
func TestDeleteTemplate_DoesNotCascadeScheduleRuns(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	tmpl, err := st.CreateTemplate(ctx, TemplateCreateSpec{ID: "tpl_doomed", Name: "doomed"})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	run, err := st.CreateScheduleRun(ctx, tmpl.ID, "agt_1")
	if err != nil {
		t.Fatalf("CreateScheduleRun: %v", err)
	}

	if err := st.DeleteTemplate(ctx, tmpl.ID); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}

	got, err := st.GetScheduleRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetScheduleRun after DeleteTemplate: %v, want the run to survive", err)
	}
	if got.TemplateID != tmpl.ID {
		t.Errorf("surviving run's TemplateID = %q, want %q", got.TemplateID, tmpl.ID)
	}
}
```

(`newStore(t, maxAgents int)` is `template_test.go`'s own existing
BoltDB-in-a-tempdir test helper, confirmed from its current tests — e.g.
`TestCreateTemplate_InsertsARecord` calls `newStore(t, 1)`. Do not invent a
second one.)

- [ ] **Step 2: Run the tests to verify they fail**

```sh
cd /workspace/ai-sandbox/docker-operator
DP=$(cat /workspace/dependaproxy-ip); DIND=$(node -e 'require("dns").lookup("docker",(e,a)=>console.log(a))')
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host="dependaproxy:$DP" --add-host="docker:$DIND" --env-file /workspace/go.env \
  -e HOME=/work/.gocache-docker-operator -e GOCACHE=/work/.gocache-docker-operator/build \
  -e GOMODCACHE=/work/.gocache-docker-operator/mod -e GOBIN=/work/.gocache-docker-operator/bin \
  -e PATH=/work/.gocache-docker-operator/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
  golang:1.26 go test ./internal/store/... -run 'TestTemplate_ScheduleFieldsRoundTrip|TestTemplate_PreScheduleRecordDecodesInert|TestScheduleRun' -v
```

Expected: a compile error (`unknown field Harness in struct literal`,
`undefined: ScheduleRunStatusRunning`, `undefined: CreateScheduleRun`, etc.)
— none of this exists yet.

- [ ] **Step 3: Add the five fields to `Template` and `TemplateCreateSpec`**

In `internal/store/template.go`, extend the `Template` struct (after
`AutoMode`):

```go
	// Harness is the CLI harness a scheduled run uses: config.HarnessClaudeCode
	// or config.HarnessOpenCode. Unlike every other field above, this one has
	// no agent-creation-form analog recorded verbatim -- templates had no
	// Harness field before this feature, since a human always picked it
	// separately in the create form even after applying a template. A cron
	// trigger has no human present to make that choice.
	Harness string `json:"harness,omitempty"`

	// Schedule is a standard 5-field cron expression ("0 9 * * *"), parsed
	// with github.com/robfig/cron/v3's ParseStandard. Empty means this
	// template is NOT a scheduled job -- it behaves exactly as every
	// template did before this feature, a pure create-form prefill.
	Schedule string `json:"schedule,omitempty"`
	// Enabled gates whether a non-empty Schedule actually fires. Ignored
	// when Schedule is empty.
	Enabled bool `json:"enabled,omitempty"`
	// Task is the prompt text passed to the harness's one-shot invocation
	// (`claude -p <Task>`) on each triggered run. Required (enforced by
	// internal/api, not the store) when Schedule is non-empty; ignored
	// otherwise.
	Task string `json:"task,omitempty"`
	// OnComplete decides what happens to a scheduled run's agent once its
	// task finishes: "stop", "delete", or "leave". Required (enforced by
	// internal/api) when Schedule is non-empty; ignored otherwise.
	OnComplete string `json:"on_complete,omitempty"`
```

Extend `TemplateCreateSpec` the same way (after its own `AutoMode`):

```go
	Harness    string
	Schedule   string
	Enabled    bool
	Task       string
	OnComplete string
```

In `CreateTemplate`, add the five fields to the `tmpl := Template{...}`
literal:

```go
		Harness:              spec.Harness,
		Schedule:             spec.Schedule,
		Enabled:              spec.Enabled,
		Task:                 spec.Task,
		OnComplete:           spec.OnComplete,
```

- [ ] **Step 4: Extend `UpdateTemplate`'s field copy in `internal/agent/templates.go`**

In `internal/agent/templates.go`, inside `UpdateTemplate`'s mutator
closure, add the five new assignments (after `t.AutoMode = spec.AutoMode`):

```go
		t.Harness = spec.Harness
		t.Schedule = spec.Schedule
		t.Enabled = spec.Enabled
		t.Task = spec.Task
		t.OnComplete = spec.OnComplete
```

- [ ] **Step 5: Create `internal/store/schedulerun.go`**

```go
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"go.etcd.io/bbolt"
)

// ErrScheduleRunNotFound reports that no ScheduleRun with the given ID
// exists. GetScheduleRun and FinishScheduleRun wrap it; callers test with
// IsScheduleRunNotFound.
var ErrScheduleRunNotFound = errors.New("schedule run not found")

// IsScheduleRunNotFound reports whether err was caused by a missing
// ScheduleRun record.
func IsScheduleRunNotFound(err error) bool { return errors.Is(err, ErrScheduleRunNotFound) }

const (
	scheduleRunIDPrefix = "run_"
	scheduleRunIDBytes  = 4
)

// ScheduleRunStatusRunning/Success/Error are the only three valid values
// for ScheduleRun.Status.
const (
	ScheduleRunStatusRunning = "running"
	ScheduleRunStatusSuccess = "success"
	ScheduleRunStatusError   = "error"
)

// bucketScheduleRuns holds one record per triggered scheduled run, keyed by
// ScheduleRun ID. Deliberately a separate bucket from bucketTemplates: a
// run's history outlives both the agent it created (which may be deleted,
// per the template's own OnComplete policy) and the template itself (which
// a human may delete later) -- see ScheduleRun's own doc comment.
var bucketScheduleRuns = []byte("schedule_runs")

// ScheduleRun is a durable record of one triggered run of a scheduled
// Template, independent of both the Template's and the Agent's own
// lifecycle: querying live store.Agent records cannot answer "what ran
// under this schedule last week" once an agent has been deleted (the
// common case for OnComplete: "delete"), and deleting the Template itself
// must not take this history down with it.
type ScheduleRun struct {
	ID         string `json:"id"`
	TemplateID string `json:"template_id"`
	AgentID    string `json:"agent_id"`
	// StartedAt is when CreateScheduleRun ran, i.e. when the trigger fired.
	StartedAt time.Time `json:"started_at"`
	// FinishedAt is nil while Status is ScheduleRunStatusRunning; set by
	// FinishScheduleRun.
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Status: ScheduleRunStatusRunning | ScheduleRunStatusSuccess |
	// ScheduleRunStatusError.
	Status string `json:"status"`
}

// NewScheduleRunID returns a fresh ID of the form "run_7f3a9c2d", mirroring
// NewTemplateID/NewID.
func NewScheduleRunID() (string, error) {
	var b [scheduleRunIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a schedule run id: %w", err)
	}
	return scheduleRunIDPrefix + hex.EncodeToString(b[:]), nil
}

// CreateScheduleRun inserts a new ScheduleRun record with Status
// ScheduleRunStatusRunning and StartedAt now, for templateID's trigger that
// just created agentID.
func (s *Store) CreateScheduleRun(ctx context.Context, templateID, agentID string) (ScheduleRun, error) {
	id, err := NewScheduleRunID()
	if err != nil {
		return ScheduleRun{}, fmt.Errorf("creating a schedule run: %w", err)
	}
	run := ScheduleRun{
		ID:         id,
		TemplateID: templateID,
		AgentID:    agentID,
		StartedAt:  s.now(),
		Status:     ScheduleRunStatusRunning,
	}
	if err := s.updateScheduleRuns(ctx, func(b *bbolt.Bucket) error {
		return putScheduleRun(b, run)
	}); err != nil {
		return ScheduleRun{}, err
	}
	return run, nil
}

// FinishScheduleRun sets the run's Status and FinishedAt (to now), or
// returns an error satisfying IsScheduleRunNotFound. Deliberately not a
// generic mutator like UpdateTemplate's: a run has exactly one transition
// (running -> finished), so a direct two-field write is simpler and
// carries no risk of a caller forgetting to preserve an unrelated field.
func (s *Store) FinishScheduleRun(ctx context.Context, id, status string) (ScheduleRun, error) {
	var updated ScheduleRun
	err := s.updateScheduleRuns(ctx, func(b *bbolt.Bucket) error {
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("finishing schedule run %q: %w", id, ErrScheduleRunNotFound)
		}
		var run ScheduleRun
		if err := decodeScheduleRun(id, raw, &run); err != nil {
			return err
		}
		now := s.now()
		run.FinishedAt = &now
		run.Status = status
		if err := putScheduleRun(b, run); err != nil {
			return err
		}
		updated = run
		return nil
	})
	if err != nil {
		return ScheduleRun{}, err
	}
	return updated, nil
}

// GetScheduleRun returns the run with the given ID, or an error satisfying
// IsScheduleRunNotFound.
func (s *Store) GetScheduleRun(ctx context.Context, id string) (ScheduleRun, error) {
	var run ScheduleRun
	err := s.viewScheduleRuns(ctx, func(b *bbolt.Bucket) error {
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("getting schedule run %q: %w", id, ErrScheduleRunNotFound)
		}
		return decodeScheduleRun(id, raw, &run)
	})
	if err != nil {
		return ScheduleRun{}, err
	}
	return run, nil
}

// ListScheduleRunsByTemplate returns every run for templateID, newest
// StartedAt first -- the order both the UI's run-history list and the
// ticker's "anchor on the most recent run" lookup want.
func (s *Store) ListScheduleRunsByTemplate(ctx context.Context, templateID string) ([]ScheduleRun, error) {
	runs := make([]ScheduleRun, 0)
	err := s.viewScheduleRuns(ctx, func(b *bbolt.Bucket) error {
		return b.ForEach(func(k, v []byte) error {
			var run ScheduleRun
			if err := decodeScheduleRun(string(k), v, &run); err != nil {
				return err
			}
			if run.TemplateID == templateID {
				runs = append(runs, run)
			}
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing schedule runs for template %q: %w", templateID, err)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	return runs, nil
}

// ListScheduleRunsByStatus returns every run with the given Status, in no
// particular order -- the ticker's completion check iterates every
// ScheduleRunStatusRunning record regardless of which template it belongs
// to.
func (s *Store) ListScheduleRunsByStatus(ctx context.Context, status string) ([]ScheduleRun, error) {
	runs := make([]ScheduleRun, 0)
	err := s.viewScheduleRuns(ctx, func(b *bbolt.Bucket) error {
		return b.ForEach(func(k, v []byte) error {
			var run ScheduleRun
			if err := decodeScheduleRun(string(k), v, &run); err != nil {
				return err
			}
			if run.Status == status {
				runs = append(runs, run)
			}
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing schedule runs with status %q: %w", status, err)
	}
	return runs, nil
}

func (s *Store) viewScheduleRuns(ctx context.Context, fn func(*bbolt.Bucket) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(func(tx *bbolt.Tx) error {
		b, err := s.scheduleRunsBucket(tx)
		if err != nil {
			return err
		}
		return fn(b)
	})
}

func (s *Store) updateScheduleRuns(ctx context.Context, fn func(*bbolt.Bucket) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := s.scheduleRunsBucket(tx)
		if err != nil {
			return err
		}
		return fn(b)
	})
}

func (s *Store) scheduleRunsBucket(tx *bbolt.Tx) (*bbolt.Bucket, error) {
	b := tx.Bucket(bucketScheduleRuns)
	if b == nil {
		return nil, fmt.Errorf("the %q bucket is missing from the state database %q", bucketScheduleRuns, s.path)
	}
	return b, nil
}

func putScheduleRun(b *bbolt.Bucket, run ScheduleRun) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("encoding schedule run %q: %w", run.ID, err)
	}
	if err := b.Put([]byte(run.ID), raw); err != nil {
		return fmt.Errorf("writing schedule run %q: %w", run.ID, err)
	}
	return nil
}

func decodeScheduleRun(id string, raw []byte, run *ScheduleRun) error {
	if err := json.Unmarshal(raw, run); err != nil {
		return fmt.Errorf("decoding schedule run %q from the state database: %w", id, err)
	}
	return nil
}
```

Add the `"time"` import to this new file's import block (it uses
`time.Time` in the `ScheduleRun` struct) — the block above already lists
every OTHER needed import; add `"time"` alongside them.

- [ ] **Step 6: Register the new bucket in `internal/store/store.go`**

Change:

```go
		for _, name := range [][]byte{bucketAgents, bucketSettings, bucketTemplates, bucketAnthropicAccounts} {
```

to:

```go
		for _, name := range [][]byte{bucketAgents, bucketSettings, bucketTemplates, bucketAnthropicAccounts, bucketScheduleRuns} {
```

- [ ] **Step 7: Run the tests and verify they pass**

```sh
cd /workspace/ai-sandbox/docker-operator
DP=$(cat /workspace/dependaproxy-ip); DIND=$(node -e 'require("dns").lookup("docker",(e,a)=>console.log(a))')
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host="dependaproxy:$DP" --add-host="docker:$DIND" --env-file /workspace/go.env \
  -e HOME=/work/.gocache-docker-operator -e GOCACHE=/work/.gocache-docker-operator/build \
  -e GOMODCACHE=/work/.gocache-docker-operator/mod -e GOBIN=/work/.gocache-docker-operator/bin \
  -e PATH=/work/.gocache-docker-operator/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
  golang:1.26 sh -c "gofmt -l . && go vet ./internal/store/... ./internal/agent/... && go test ./internal/store/... ./internal/agent/... -race"
```

Expected: `gofmt -l` prints nothing, no vet warnings, all tests `PASS`.

- [ ] **Step 8: Commit**

```bash
cd /workspace/ai-sandbox
git add docker-operator/internal/store/template.go docker-operator/internal/store/template_test.go \
  docker-operator/internal/store/schedulerun.go docker-operator/internal/store/schedulerun_test.go \
  docker-operator/internal/store/store.go docker-operator/internal/agent/templates.go
git commit -m "$(cat <<'EOF'
feat(store): add Template scheduling fields and the ScheduleRun record

Template gains Harness/Schedule/Enabled/Task/OnComplete -- the data
model a cron-triggered run needs, extending the existing
create-form-prefill concept rather than inventing a new one. A new
ScheduleRun record tracks one run per trigger, independent of the
agent's own lifecycle, so run history survives both OnComplete:
"delete" and the template being removed later.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: `internal/agent` — schedule parsing, one-shot creation, and completion

**Files:**
- Create: `internal/agent/schedule.go`
- Create: `internal/agent/schedule_test.go`
- Modify: `internal/agent/create.go` (add `CreateRequest.OneShotTask`,
  thread it through `build`, ~line 358 and ~line 664)
- Modify: `internal/agent/harness.go` (add a one-shot args helper near
  `firstInvocationArgs`, ~line 93)
- Modify: `go.mod` / `go.sum` (new dependency)

**Interfaces:**
- Consumes: `store.Template` (Task 1's new fields);
  `store.ScheduleRunStatusRunning/Success/Error`,
  `(*store.Store) ListScheduleRunsByTemplate`,
  `(*store.Store) CreateScheduleRun`, `(*store.Store) FinishScheduleRun`
  (Task 1); `dockerclient.Client.ExecCreate/ExecAttach` and `m.docker`,
  `m.opts.ExecTimeout` (both already exist; the same `ExecClient` surface
  `waitTmuxSession` already uses, in `internal/agent/create.go` —
  `execOneShot` is new and independent of it, not a refactor of it, per
  Step 7); `firstNonEmpty` (already exists, `internal/agent/delete.go`);
  `config.HarnessOpenCode`, `config.ValidHarness` (already exist).
- Produces: `ParseSchedule(expr string) (cron.Schedule, error)` (wraps
  `cron.ParseStandard`; used by both this task's own due-check and Task
  4's API validation); `TemplateDue(tmpl store.Template, lastRun
  *store.ScheduleRun, now time.Time) bool` (pure function, no store/ticker
  involved — Task 3 supplies `lastRun` itself); `(m *Manager)
  CreateFromTemplate(ctx context.Context, tmpl store.Template) (store.Agent,
  error)` (builds a `CreateRequest` from `tmpl` and calls `m.Create`, with
  `OneShotTask` set from `tmpl.Task`); `(m *Manager)
  CheckScheduledRunStatus(ctx context.Context, run store.ScheduleRun)
  (status string, err error)` (execs the sentinel-file check; `status` is
  one of `"running"`, `"done"`, or `"gone"` — `"gone"` when the agent or
  container no longer exists, which the ticker finalizes as an error
  rather than leaving the run stuck "running" forever — see Step 8);
  `(m *Manager) ApplyOnComplete(ctx context.Context, tmpl
  store.Template, run store.ScheduleRun) error` (stop/delete/leave,
  dispatched on `tmpl.OnComplete`); `(m *Manager)
  ListScheduleRunsByTemplate(ctx context.Context, templateID string)
  ([]store.ScheduleRun, error)` (thin pass-through to `m.store`'s own
  method, mirroring `ListTemplates`'s shape — added here rather than in
  Task 4 because Task 3's `schedulingDeps` interface needs it on `Manager`
  first); `CreateRequest.OneShotTask string` (new field: when non-empty,
  the agent's first invocation runs `claude -p "<OneShotTask>"` instead of
  an interactive session).
- Produces (constant): `taskCompleteSentinelPath = "/workspace/.task-complete"`.

- [ ] **Step 1: Add the `robfig/cron/v3` dependency**

```sh
cd /workspace/ai-sandbox/docker-operator
DP=$(cat /workspace/dependaproxy-ip)
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host="dependaproxy:$DP" --env-file /workspace/go.env \
  -e HOME=/work/.gocache-docker-operator -e GOCACHE=/work/.gocache-docker-operator/build \
  -e GOMODCACHE=/work/.gocache-docker-operator/mod -e GOBIN=/work/.gocache-docker-operator/bin \
  -e PATH=/work/.gocache-docker-operator/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local -e GOPROXY=http://dependaproxy:8080/goproxy \
  golang:1.26 go get github.com/robfig/cron/v3@v3.0.1
```

Expected: `go.mod` gains a `require github.com/robfig/cron/v3 v3.0.1`
line and `go.sum` gains matching entries. If DependaProxy's validation
gate 403s this version, check its advisory message (per the
`use-dependaproxy` skill) and retry with whatever version it names as
clean — do not bypass the gate.

- [ ] **Step 2: Write the failing tests**

Create `internal/agent/schedule_test.go`:

```go
package agent

import (
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

func TestParseSchedule_Valid(t *testing.T) {
	sched, err := ParseSchedule("0 9 * * *")
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	anchor := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	next := sched.Next(anchor)
	want := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("Next(%v) = %v, want %v", anchor, next, want)
	}
}

func TestParseSchedule_Invalid(t *testing.T) {
	if _, err := ParseSchedule("not a cron expression"); err == nil {
		t.Error("ParseSchedule(garbage) = nil error, want non-nil")
	}
	if _, err := ParseSchedule("* * * * * *"); err == nil {
		t.Error("ParseSchedule(6 fields, with seconds) = nil error, want non-nil (standard parser is 5 fields)")
	}
}

func TestTemplateDue_NeverRun(t *testing.T) {
	tmpl := store.Template{
		Schedule:  "0 9 * * *",
		Enabled:   true,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	if !TemplateDue(tmpl, nil, now) {
		t.Error("TemplateDue = false, want true (9am fired since CreatedAt at midnight, now is 10am)")
	}
	if TemplateDue(tmpl, nil, time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)) {
		t.Error("TemplateDue = true, want false (9am has not happened yet)")
	}
}

func TestTemplateDue_AnchorsOnLastRun(t *testing.T) {
	tmpl := store.Template{Schedule: "0 9 * * *", Enabled: true}
	lastRun := &store.ScheduleRun{StartedAt: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	// The next 9am after the last run's 9am is the FOLLOWING day -- today's
	// 9am already fired and produced lastRun.
	if TemplateDue(tmpl, lastRun, time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)) {
		t.Error("TemplateDue = true, want false (already ran today)")
	}
	if !TemplateDue(tmpl, lastRun, time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)) {
		t.Error("TemplateDue = false, want true (tomorrow's 9am has arrived)")
	}
}

func TestTemplateDue_DisabledNeverFires(t *testing.T) {
	tmpl := store.Template{Schedule: "0 9 * * *", Enabled: false}
	if TemplateDue(tmpl, nil, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("TemplateDue(disabled) = true, want false regardless of how far in the future now is")
	}
}

func TestTemplateDue_NoScheduleNeverFires(t *testing.T) {
	tmpl := store.Template{Schedule: "", Enabled: true}
	if TemplateDue(tmpl, nil, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("TemplateDue(no schedule) = true, want false")
	}
}
```

Add to `internal/agent/create_test.go` (or a new `oneshot_test.go` beside
it — this plan uses the existing file, since the assertion is about
`firstInvocationArgs`'s sibling, not a new file's worth of concern):

```go
func TestOneShotInvocationArgs(t *testing.T) {
	args := oneShotInvocationArgs(store.Agent{}, "do the thing")
	want := []string{"-p", "do the thing"}
	if len(args) != len(want) || args[0] != want[0] || args[1] != want[1] {
		t.Errorf("oneShotInvocationArgs = %v, want %v", args, want)
	}
}

func TestOneShotInvocationArgs_AutoMode(t *testing.T) {
	args := oneShotInvocationArgs(store.Agent{AutoMode: config.AutoModeOn}, "do the thing")
	want := []string{"--permission-mode", "auto", "-p", "do the thing"}
	if len(args) != len(want) {
		t.Fatalf("oneShotInvocationArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("oneShotInvocationArgs = %v, want %v", args, want)
		}
	}
}
```

Create a new test file `internal/agent/completion_test.go` for
`CreateFromTemplate`, `CheckScheduledRunStatus`, and `ApplyOnComplete`
(these need the full test-manager harness, unlike the pure functions
above):

```go
package agent

import (
	"context"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

func TestCreateFromTemplate(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	ctx := context.Background()

	tmpl := store.Template{
		Harness:  config.HarnessClaudeCode,
		Backend:  config.BackendOllama,
		Task:     "do the daily thing",
		Schedule: "0 9 * * *",
	}
	a, err := m.CreateFromTemplate(ctx, tmpl)
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	if a.Harness != config.HarnessClaudeCode {
		t.Errorf("Harness = %q, want %q", a.Harness, config.HarnessClaudeCode)
	}
}

func TestCheckScheduledRunStatus_NotDoneYet(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	tmpl := store.Template{Harness: config.HarnessClaudeCode, Backend: config.BackendOllama, Task: "x"}
	a, err := m.CreateFromTemplate(ctx, tmpl)
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	f.ExecOutput["sh -c [ -f '"+taskCompleteSentinelPath+"' ] && echo present"] = nil

	run := store.ScheduleRun{AgentID: a.ID}
	status, err := m.CheckScheduledRunStatus(ctx, run)
	if err != nil {
		t.Fatalf("CheckScheduledRunStatus: %v", err)
	}
	if status != "running" {
		t.Errorf("status = %q, want %q (sentinel not seeded)", status, "running")
	}
}

func TestCheckScheduledRunStatus_Done(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	tmpl := store.Template{Harness: config.HarnessClaudeCode, Backend: config.BackendOllama, Task: "x"}
	a, err := m.CreateFromTemplate(ctx, tmpl)
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	f.ExecOutput["sh -c [ -f '"+taskCompleteSentinelPath+"' ] && echo present"] = []byte("present\n")

	run := store.ScheduleRun{AgentID: a.ID}
	status, err := m.CheckScheduledRunStatus(ctx, run)
	if err != nil {
		t.Fatalf("CheckScheduledRunStatus: %v", err)
	}
	if status != "done" {
		t.Errorf("status = %q, want %q (sentinel present)", status, "done")
	}
}

// TestCheckScheduledRunStatus_AgentGone pins the Review Focus item: a
// ScheduleRun whose agent was deleted out-of-band must report "gone", not
// "running" and not an error -- so the ticker can finalize it as error
// rather than checking it forever.
func TestCheckScheduledRunStatus_AgentGone(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	ctx := context.Background()

	run := store.ScheduleRun{AgentID: "agt_does_not_exist"}
	status, err := m.CheckScheduledRunStatus(ctx, run)
	if err != nil {
		t.Fatalf("CheckScheduledRunStatus(gone): %v, want nil error", err)
	}
	if status != "gone" {
		t.Errorf("status = %q, want %q for a gone agent", status, "gone")
	}
}

func TestApplyOnComplete_Stop(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	tmpl := store.Template{Harness: config.HarnessClaudeCode, Backend: config.BackendOllama, Task: "x", OnComplete: "stop"}
	a, err := m.CreateFromTemplate(ctx, tmpl)
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	run := store.ScheduleRun{AgentID: a.ID}
	if err := m.ApplyOnComplete(ctx, tmpl, run); err != nil {
		t.Fatalf("ApplyOnComplete: %v", err)
	}

	c, err := f.ContainerInspect(ctx, agentContainerName(a.ID))
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if c.State != dockerclient.StateStopped {
		t.Errorf("container state = %v, want stopped", c.State)
	}
}

func TestApplyOnComplete_Delete(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()

	tmpl := store.Template{Harness: config.HarnessClaudeCode, Backend: config.BackendOllama, Task: "x", OnComplete: "delete"}
	a, err := m.CreateFromTemplate(ctx, tmpl)
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	run := store.ScheduleRun{AgentID: a.ID}
	if err := m.ApplyOnComplete(ctx, tmpl, run); err != nil {
		t.Fatalf("ApplyOnComplete: %v", err)
	}

	if _, err := st.Get(ctx, a.ID); !store.IsNotFound(err) {
		t.Errorf("agent record still present after OnComplete delete: %v", err)
	}
}

func TestApplyOnComplete_Leave(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	tmpl := store.Template{Harness: config.HarnessClaudeCode, Backend: config.BackendOllama, Task: "x", OnComplete: "leave"}
	a, err := m.CreateFromTemplate(ctx, tmpl)
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	run := store.ScheduleRun{AgentID: a.ID}
	if err := m.ApplyOnComplete(ctx, tmpl, run); err != nil {
		t.Fatalf("ApplyOnComplete: %v", err)
	}

	c, err := f.ContainerInspect(ctx, agentContainerName(a.ID))
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if c.State != dockerclient.StateRunning {
		t.Errorf("container state = %v, want still running (leave)", c.State)
	}
}

func TestListScheduleRunsByTemplate(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()

	if _, err := st.CreateScheduleRun(ctx, "tpl_1", "agt_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateScheduleRun(ctx, "tpl_2", "agt_2"); err != nil {
		t.Fatal(err)
	}

	got, err := m.ListScheduleRunsByTemplate(ctx, "tpl_1")
	if err != nil {
		t.Fatalf("ListScheduleRunsByTemplate: %v", err)
	}
	if len(got) != 1 || got[0].TemplateID != "tpl_1" {
		t.Errorf("ListScheduleRunsByTemplate(tpl_1) = %+v, want exactly tpl_1's run", got)
	}
}
```

Add `"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"`
to this test file's import block for `dockerclient.StateStopped`/
`dockerclient.StateRunning`.

- [ ] **Step 3: Run the tests to verify they fail**

```sh
cd /workspace/ai-sandbox/docker-operator
DP=$(cat /workspace/dependaproxy-ip); DIND=$(node -e 'require("dns").lookup("docker",(e,a)=>console.log(a))')
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host="dependaproxy:$DP" --add-host="docker:$DIND" --env-file /workspace/go.env \
  -e HOME=/work/.gocache-docker-operator -e GOCACHE=/work/.gocache-docker-operator/build \
  -e GOMODCACHE=/work/.gocache-docker-operator/mod -e GOBIN=/work/.gocache-docker-operator/bin \
  -e PATH=/work/.gocache-docker-operator/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
  golang:1.26 go test ./internal/agent/... -run 'TestParseSchedule|TestTemplateDue|TestOneShotInvocationArgs|TestCreateFromTemplate|TestCheckScheduledRunStatus|TestApplyOnComplete' -v
```

Expected: a compile error (`undefined: ParseSchedule`, `undefined:
TemplateDue`, `undefined: oneShotInvocationArgs`, `undefined:
CreateFromTemplate`, etc.).

- [ ] **Step 4: Add `oneShotInvocationArgs` to `internal/agent/harness.go`**

```go
// oneShotInvocationArgs builds the CLI args for a one-shot, non-interactive
// run: the harness's auto-mode flag (if any), then "-p" and task verbatim.
// Unlike firstInvocationArgs, this is not a session-resumption concern at
// all -- a one-shot run is inherently fresh, never continued or resumed --
// so it is a separate function rather than a fourth sessionStart case.
// Claude Code only (config.HarnessOpenCode is rejected for a scheduled
// template at the API layer before this is ever called with one).
func oneShotInvocationArgs(a store.Agent, task string) []string {
	args := autoModeArgs(a)
	return append(args, "-p", task)
}
```

- [ ] **Step 5: Thread `OneShotTask` through `CreateRequest` and `build`**

In `internal/agent/create.go`, add to `CreateRequest` (after `AccountID`):

```go
	// OneShotTask, when non-empty, makes the agent's first invocation run
	// claude -p "<OneShotTask>" instead of starting an interactive session.
	// Set by CreateFromTemplate from a scheduled Template's Task field;
	// empty for every ordinary human-initiated create.
	OneShotTask string
```

Change `build`'s signature and its `startAgentContainer` call:

```go
func (m *Manager) build(ctx context.Context, a *store.Agent, rb resolvedBackend, oneShotTask string) error {
```

```go
	args := firstInvocationArgs(*a, sessionFresh)
	if oneShotTask != "" {
		args = oneShotInvocationArgs(*a, oneShotTask)
	}
	if err := m.startAgentContainer(ctx, a, rb, args...); err != nil {
		return err
	}
```

Update `build`'s one call site in `Create`:

```go
	if err := m.build(ctx, &a, rs.rb, req.OneShotTask); err != nil {
```

- [ ] **Step 6: Create `internal/agent/schedule.go`**

```go
package agent

import (
	"context"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// taskCompleteSentinelPath is the file claude-supervisor.sh writes right
// after a clean (status 0) one-shot exit -- see agent/claude-supervisor.sh.
// Same directory and naming convention as wsbridge.OutputLogPath's
// /workspace/.agent-output.log.
const taskCompleteSentinelPath = "/workspace/.task-complete"

// ParseSchedule parses a standard 5-field cron expression. Used both by
// the ticker (via TemplateDue, below) and by internal/api's template
// validation, so a Template's Schedule is checked with the exact same
// parser it will later be run against.
func ParseSchedule(expr string) (cron.Schedule, error) {
	return cron.ParseStandard(expr)
}

// TemplateDue reports whether tmpl's schedule has a fire time in
// (anchor, now], where anchor is lastRun's StartedAt if non-nil, else
// tmpl.CreatedAt. A disabled or unscheduled template is never due. An
// unparseable Schedule (should not happen past internal/api's validation,
// but this function makes no assumption about that) is treated as never
// due, not a panic.
func TemplateDue(tmpl store.Template, lastRun *store.ScheduleRun, now time.Time) bool {
	if !tmpl.Enabled || tmpl.Schedule == "" {
		return false
	}
	sched, err := ParseSchedule(tmpl.Schedule)
	if err != nil {
		return false
	}
	anchor := tmpl.CreatedAt
	if lastRun != nil {
		anchor = lastRun.StartedAt
	}
	next := sched.Next(anchor)
	return !next.After(now)
}

// CreateFromTemplate creates a fresh agent from tmpl's fields, running
// tmpl.Task as a one-shot invocation. This translation is NEW: the only
// existing Template -> create-agent-fields mapping before this feature was
// web/app.js's applyTemplateToForm, pure client-side JS that prefills the
// create form for a human to then submit -- there was no server-side
// equivalent, since nothing before this feature ever created an agent from
// a template without a human in the loop.
func (m *Manager) CreateFromTemplate(ctx context.Context, tmpl store.Template) (store.Agent, error) {
	return m.Create(ctx, CreateRequest{
		Backend:              tmpl.Backend,
		Harness:              tmpl.Harness,
		Model:                tmpl.Model,
		FastModel:            tmpl.FastModel,
		OllamaURL:            tmpl.OllamaURL,
		Repo:                 tmpl.Repo,
		AutoCompactThreshold: tmpl.AutoCompactThreshold,
		MaxContextTokens:     tmpl.MaxContextTokens,
		ImageTag:             tmpl.ImageTag,
		AutoMode:             tmpl.AutoMode,
		OneShotTask:          tmpl.Task,
	})
}

// CheckScheduledRunStatus reports what state run's agent is actually in,
// by execing a check for taskCompleteSentinelPath via execOneShot:
//
//   - "running": the agent exists and the sentinel is not there yet.
//   - "done": the sentinel is present -- the one-shot task finished cleanly.
//   - "gone": the agent or container no longer exists (deleted out-of-band,
//     or by an earlier, crashed OnComplete pass).
//
// "gone" is reported with a nil error, deliberately: it is NOT a transient
// failure worth retrying, it is the ticker's cue to finalize that
// ScheduleRun as an error itself -- collapsing it into the same result as
// "running" (as an earlier draft of this function did, returning a bare
// bool) would leave a run whose agent is gone stuck "running" forever,
// re-checked every tick with nothing ever finalizing it.
func (m *Manager) CheckScheduledRunStatus(ctx context.Context, run store.ScheduleRun) (string, error) {
	ref := agentContainerName(run.AgentID)
	cmd := []string{"sh", "-c", "[ -f '" + taskCompleteSentinelPath + "' ] && echo present"}
	out, err := m.execOneShot(ctx, ref, cmd)
	if err != nil {
		if dockerclient.IsNotFound(err) {
			return "gone", nil
		}
		return "", err
	}
	if len(out) > 0 {
		return "done", nil
	}
	return "running", nil
}

// ApplyOnComplete runs tmpl's completion policy for run's agent: "stop"
// (ContainerStop, left inspectable), "delete" (Delete, which archives the
// transcript first per the already-shipped internal/agent/transcript.go,
// regardless of this feature), or "leave" (nothing further). An unknown or
// empty OnComplete is treated as "leave" -- the safest no-op default,
// never reachable past internal/api's validation but defensive here too.
func (m *Manager) ApplyOnComplete(ctx context.Context, tmpl store.Template, run store.ScheduleRun) error {
	switch tmpl.OnComplete {
	case "stop":
		return m.docker.ContainerStop(ctx, agentContainerName(run.AgentID), m.opts.StopTimeout)
	case "delete":
		return m.Delete(ctx, run.AgentID)
	default:
		return nil
	}
}

// ListScheduleRunsByTemplate returns every run for templateID, newest
// first, mirroring ListTemplates's thin pass-through shape. Added here,
// not in internal/api's task, because cmd/docker-operator's
// schedulingDeps interface (next task) needs this on Manager before that
// task ever runs -- internal/api's own task only calls it, it does not
// define it.
func (m *Manager) ListScheduleRunsByTemplate(ctx context.Context, templateID string) ([]store.ScheduleRun, error) {
	return m.store.ListScheduleRunsByTemplate(ctx, templateID)
}
```

- [ ] **Step 7: Add `execOneShot`, a new self-contained helper**

`waitTmuxSession` (in `internal/agent/create.go`, ~line 1404) already runs
a one-shot exec and polls `ExecInspect` for its exit status — a more
involved shape than this feature needs, since all `CheckScheduledRunStatus`
needs is "run this command, read what it printed." Rather than risk
changing `waitTmuxSession`'s existing, working timeout/polling behavior to
extract a shared core from it, add a new, independent helper instead —
bounded by its own `context.WithTimeout` rather than depending on
`waitTmuxSession`'s internals at all:

```go
// execOneShot runs cmd inside the container named ref via one exec,
// bounded by its own timeout (m.opts.ExecTimeout), and returns everything
// the command wrote to stdout/stderr. A missing container satisfies
// dockerclient.IsNotFound. Deliberately independent of waitTmuxSession's
// own exec-and-poll-ExecInspect shape (internal/agent/create.go) rather
// than refactored out of it: that function's exit-status polling is more
// than this helper's callers need, and changing it risks a regression in
// an already-working path for no benefit this feature requires.
func (m *Manager) execOneShot(ctx context.Context, ref string, cmd []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, m.opts.ExecTimeout)
	defer cancel()

	execID, err := m.docker.ExecCreate(ctx, ref, dockerclient.ExecSpec{Cmd: cmd})
	if err != nil {
		return nil, err
	}
	stream, err := m.docker.ExecAttach(ctx, execID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	return io.ReadAll(stream)
}
```

- [ ] **Step 8: Run the tests and verify they pass**

```sh
cd /workspace/ai-sandbox/docker-operator
DP=$(cat /workspace/dependaproxy-ip); DIND=$(node -e 'require("dns").lookup("docker",(e,a)=>console.log(a))')
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host="dependaproxy:$DP" --add-host="docker:$DIND" --env-file /workspace/go.env \
  -e HOME=/work/.gocache-docker-operator -e GOCACHE=/work/.gocache-docker-operator/build \
  -e GOMODCACHE=/work/.gocache-docker-operator/mod -e GOBIN=/work/.gocache-docker-operator/bin \
  -e PATH=/work/.gocache-docker-operator/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
  golang:1.26 sh -c "gofmt -l . && go vet ./... && go test ./internal/agent/... -race"
```

Expected: `gofmt -l` prints nothing, no vet warnings, all tests `PASS`
(this also re-runs every pre-existing `internal/agent` test, since
`build`'s signature changed and `waitTmuxSession` was refactored — a
regression in either shows up here, not just in this task's new tests).

- [ ] **Step 9: Commit**

```bash
cd /workspace/ai-sandbox
git add docker-operator/go.mod docker-operator/go.sum \
  docker-operator/internal/agent/schedule.go docker-operator/internal/agent/schedule_test.go \
  docker-operator/internal/agent/completion_test.go \
  docker-operator/internal/agent/create.go docker-operator/internal/agent/create_test.go \
  docker-operator/internal/agent/harness.go
git commit -m "$(cat <<'EOF'
feat(agent): one-shot agent creation and scheduled-run completion

CreateFromTemplate builds a CreateRequest from a Template and runs
its Task as a one-shot `claude -p` invocation -- the first
server-side path that ever creates an agent from a template without
a human in the loop. CheckScheduledRunStatus polls for the
completion sentinel (next commit adds the write side) via a new,
self-contained one-shot exec helper. ApplyOnComplete dispatches a
template's stop/delete/leave policy.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: `cmd/docker-operator` — the scheduling ticker

**Files:**
- Create: `cmd/docker-operator/schedule.go`
- Create: `cmd/docker-operator/schedule_test.go`
- Modify: `cmd/docker-operator/main.go` (wire the new ticker alongside
  `startPeriodicReconcile`, ~line 149)

**Interfaces:**
- Consumes: `agent.TemplateDue`, `(m *Manager) CreateFromTemplate`,
  `(m *Manager) CheckScheduledRunStatus`, `(m *Manager)
  ApplyOnComplete` (Task 2); `store.ScheduleRunStatusRunning`,
  `(*store.Store) ListScheduleRunsByTemplate`, `(*store.Store)
  CreateScheduleRun`, `(*store.Store) FinishScheduleRun`, `(*store.Store)
  ListScheduleRunsByStatus`, `(*store.Store) ListTemplates` (Task 1 +
  already-existing).
- Produces: `startSchedulingTicker(deps schedulingDeps, interval
  time.Duration, log *slog.Logger) func()` (cancel-based stop function,
  same shape as `startPeriodicReconcile`); `schedulingDeps` (the interface
  seam this ticker needs, mirroring `reconciler`'s role for
  `startPeriodicReconcile`); `runSchedulingTickOnce(ctx context.Context,
  deps schedulingDeps, log *slog.Logger)` (the per-tick body, unit-testable
  without a real timer — same split `runPeriodicReconcileOnce` has from
  `startPeriodicReconcile`).

- [ ] **Step 1: Write the failing test**

Create `cmd/docker-operator/schedule_test.go`:

```go
package main

import (
	"context"
	"log/slog"
	"io"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// fakeSchedulingDeps is a minimal in-memory stand-in for schedulingDeps,
// recording exactly what runSchedulingTickOnce does without touching a
// real Manager or Store.
type fakeSchedulingDeps struct {
	templates      []store.Template
	runsByTemplate map[string][]store.ScheduleRun
	runningRuns    []store.ScheduleRun
	created        []string // template IDs CreateFromTemplate was called for
	statuses       map[string]string // run ID -> "running" | "done" | "gone"
	completed      []string          // run IDs ApplyOnComplete was called for
	finished       map[string]string // run ID -> status FinishScheduleRun was called with
}

func (f *fakeSchedulingDeps) ListTemplates(ctx context.Context) ([]store.Template, error) {
	return f.templates, nil
}
func (f *fakeSchedulingDeps) ListScheduleRunsByTemplate(ctx context.Context, templateID string) ([]store.ScheduleRun, error) {
	return f.runsByTemplate[templateID], nil
}
func (f *fakeSchedulingDeps) ListScheduleRunsByStatus(ctx context.Context, status string) ([]store.ScheduleRun, error) {
	if status == store.ScheduleRunStatusRunning {
		return f.runningRuns, nil
	}
	return nil, nil
}
func (f *fakeSchedulingDeps) CreateScheduleRun(ctx context.Context, templateID, agentID string) (store.ScheduleRun, error) {
	return store.ScheduleRun{ID: "run_" + templateID, TemplateID: templateID, AgentID: agentID, Status: store.ScheduleRunStatusRunning}, nil
}
func (f *fakeSchedulingDeps) FinishScheduleRun(ctx context.Context, id, status string) (store.ScheduleRun, error) {
	if f.finished == nil {
		f.finished = map[string]string{}
	}
	f.finished[id] = status
	return store.ScheduleRun{ID: id, Status: status}, nil
}
func (f *fakeSchedulingDeps) CreateFromTemplate(ctx context.Context, tmpl store.Template) (store.Agent, error) {
	f.created = append(f.created, tmpl.ID)
	return store.Agent{ID: "agt_for_" + tmpl.ID}, nil
}
func (f *fakeSchedulingDeps) CheckScheduledRunStatus(ctx context.Context, run store.ScheduleRun) (string, error) {
	return f.statuses[run.ID], nil
}
func (f *fakeSchedulingDeps) ApplyOnComplete(ctx context.Context, tmpl store.Template, run store.ScheduleRun) error {
	f.completed = append(f.completed, run.ID)
	return nil
}
func (f *fakeSchedulingDeps) GetTemplate(ctx context.Context, id string) (store.Template, error) {
	for _, t := range f.templates {
		if t.ID == id {
			return t, nil
		}
	}
	return store.Template{}, store.ErrTemplateNotFound
}

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRunSchedulingTickOnce_FiresDueTemplate(t *testing.T) {
	deps := &fakeSchedulingDeps{
		templates: []store.Template{{
			ID: "tpl_1", Enabled: true, Schedule: "0 9 * * *",
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.created) != 1 || deps.created[0] != "tpl_1" {
		t.Errorf("created = %v, want [tpl_1]", deps.created)
	}
}

func TestRunSchedulingTickOnce_SkipsNotDue(t *testing.T) {
	deps := &fakeSchedulingDeps{
		templates: []store.Template{{
			ID: "tpl_1", Enabled: true, Schedule: "0 9 * * *",
			CreatedAt: time.Now().Add(time.Hour), // created in the future: never due yet
		}},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.created) != 0 {
		t.Errorf("created = %v, want none", deps.created)
	}
}

// TestRunSchedulingTickOnce_FiresAllDueInOneTick pins the Review Focus
// item: multiple due schedules in one tick must ALL fire, and one
// template's failure must not block another's.
func TestRunSchedulingTickOnce_FiresAllDueInOneTick(t *testing.T) {
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	deps := &fakeSchedulingDeps{
		templates: []store.Template{
			{ID: "tpl_1", Enabled: true, Schedule: "0 9 * * *", CreatedAt: past},
			{ID: "tpl_2", Enabled: true, Schedule: "0 10 * * *", CreatedAt: past},
		},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.created) != 2 {
		t.Errorf("created = %v, want both tpl_1 and tpl_2", deps.created)
	}
}

func TestRunSchedulingTickOnce_ChecksCompletionAndApplies(t *testing.T) {
	deps := &fakeSchedulingDeps{
		runningRuns: []store.ScheduleRun{{ID: "run_1", TemplateID: "tpl_1", AgentID: "agt_1"}},
		statuses:    map[string]string{"run_1": "done"},
		templates:   []store.Template{{ID: "tpl_1", OnComplete: "stop"}},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.completed) != 1 || deps.completed[0] != "run_1" {
		t.Errorf("completed = %v, want [run_1]", deps.completed)
	}
	if deps.finished["run_1"] != store.ScheduleRunStatusSuccess {
		t.Errorf("finished[run_1] = %q, want %q", deps.finished["run_1"], store.ScheduleRunStatusSuccess)
	}
}

func TestRunSchedulingTickOnce_LeavesIncompleteRunsAlone(t *testing.T) {
	deps := &fakeSchedulingDeps{
		runningRuns: []store.ScheduleRun{{ID: "run_1", TemplateID: "tpl_1", AgentID: "agt_1"}},
		statuses:    map[string]string{"run_1": "running"},
		templates:   []store.Template{{ID: "tpl_1", OnComplete: "stop"}},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.completed) != 0 {
		t.Errorf("completed = %v, want none (not done yet)", deps.completed)
	}
	if len(deps.finished) != 0 {
		t.Errorf("finished = %v, want none (still running)", deps.finished)
	}
}

// TestRunSchedulingTickOnce_FinalizesGoneAgentAsError pins the Review
// Focus item: a run whose agent was deleted out-of-band must be finalized
// as an error, not left "running" forever and not retried.
func TestRunSchedulingTickOnce_FinalizesGoneAgentAsError(t *testing.T) {
	deps := &fakeSchedulingDeps{
		runningRuns: []store.ScheduleRun{{ID: "run_1", TemplateID: "tpl_1", AgentID: "agt_1"}},
		statuses:    map[string]string{"run_1": "gone"},
		templates:   []store.Template{{ID: "tpl_1", OnComplete: "stop"}},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.completed) != 0 {
		t.Errorf("completed = %v, want none -- OnComplete is not applied to a gone agent", deps.completed)
	}
	if deps.finished["run_1"] != store.ScheduleRunStatusError {
		t.Errorf("finished[run_1] = %q, want %q", deps.finished["run_1"], store.ScheduleRunStatusError)
	}
}

// TestRunSchedulingTickOnce_SkipsOpenCodeTemplate pins the Global
// Constraint that scheduling a Template whose Harness is opencode never
// runs, even if a record reached that state before internal/api's own
// validation existed to reject it -- defensive insurance at the ticker
// itself, not just at the API boundary.
func TestRunSchedulingTickOnce_SkipsOpenCodeTemplate(t *testing.T) {
	deps := &fakeSchedulingDeps{
		templates: []store.Template{{
			ID: "tpl_1", Enabled: true, Schedule: "0 9 * * *", Harness: "opencode",
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.created) != 0 {
		t.Errorf("created = %v, want none (opencode templates are rejected defensively)", deps.created)
	}
}

// TestRunSchedulingTickOnce_DeletedTemplateStillFinalizesRun pins the
// Review Focus item: a run whose template was deleted while it was still
// in flight must still be checked, still get ApplyOnComplete called (with
// the zero-value Template, whose OnComplete "" is "leave"), and still be
// finalized as success -- never stuck "running" just because its template
// is gone.
func TestRunSchedulingTickOnce_DeletedTemplateStillFinalizesRun(t *testing.T) {
	deps := &fakeSchedulingDeps{
		runningRuns: []store.ScheduleRun{{ID: "run_1", TemplateID: "tpl_deleted", AgentID: "agt_1"}},
		statuses:    map[string]string{"run_1": "done"},
		// No templates at all: tpl_deleted has been removed.
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.completed) != 1 || deps.completed[0] != "run_1" {
		t.Errorf("completed = %v, want [run_1] (ApplyOnComplete still called, with the zero-value template)", deps.completed)
	}
	if deps.finished["run_1"] != store.ScheduleRunStatusSuccess {
		t.Errorf("finished[run_1] = %q, want %q", deps.finished["run_1"], store.ScheduleRunStatusSuccess)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```sh
cd /workspace/ai-sandbox/docker-operator
DP=$(cat /workspace/dependaproxy-ip); DIND=$(node -e 'require("dns").lookup("docker",(e,a)=>console.log(a))')
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host="dependaproxy:$DP" --add-host="docker:$DIND" --env-file /workspace/go.env \
  -e HOME=/work/.gocache-docker-operator -e GOCACHE=/work/.gocache-docker-operator/build \
  -e GOMODCACHE=/work/.gocache-docker-operator/mod -e GOBIN=/work/.gocache-docker-operator/bin \
  -e PATH=/work/.gocache-docker-operator/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
  golang:1.26 go test ./cmd/docker-operator/... -run TestRunSchedulingTickOnce -v
```

Expected: a compile error (`undefined: runSchedulingTickOnce`).

- [ ] **Step 3: Create `cmd/docker-operator/schedule.go`**

```go
package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// schedulingDeps is the Manager/Store surface runSchedulingTickOnce needs,
// as an interface seam -- the same role reconciler plays for
// startPeriodicReconcile -- so schedule_test.go can drive it with an
// in-memory fake instead of a real Manager and BoltDB file.
type schedulingDeps interface {
	ListTemplates(ctx context.Context) ([]store.Template, error)
	GetTemplate(ctx context.Context, id string) (store.Template, error)
	ListScheduleRunsByTemplate(ctx context.Context, templateID string) ([]store.ScheduleRun, error)
	ListScheduleRunsByStatus(ctx context.Context, status string) ([]store.ScheduleRun, error)
	CreateScheduleRun(ctx context.Context, templateID, agentID string) (store.ScheduleRun, error)
	FinishScheduleRun(ctx context.Context, id, status string) (store.ScheduleRun, error)
	CreateFromTemplate(ctx context.Context, tmpl store.Template) (store.Agent, error)
	CheckScheduledRunStatus(ctx context.Context, run store.ScheduleRun) (status string, err error)
	ApplyOnComplete(ctx context.Context, tmpl store.Template, run store.ScheduleRun) error
}

// startSchedulingTicker fires due schedules and checks in-flight runs for
// completion every interval, modeled directly on startPeriodicReconcile:
// a cancel-based stop function, overlap-guarded the same structural way
// (a tick never re-enters while the previous one is still inline).
func startSchedulingTicker(deps schedulingDeps, interval time.Duration, log *slog.Logger) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		var running atomic.Bool
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !running.CompareAndSwap(false, true) {
					log.Info("scheduling tick: the previous tick is still running; skipping this one")
					continue
				}
				runSchedulingTickOnce(ctx, deps, log)
				running.Store(false)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// runSchedulingTickOnce is one tick's body: fire every due, enabled
// template, then check every in-flight run for completion. Each half
// processes every eligible item regardless of an earlier one's failure --
// one template's CreateFromTemplate error, or one run's completion-check
// error, is logged and the loop continues, mirroring teardown's own
// "collect every error, never stop at the first" stance.
func runSchedulingTickOnce(ctx context.Context, deps schedulingDeps, log *slog.Logger) {
	fireDueTemplates(ctx, deps, log)
	checkRunningSchedules(ctx, deps, log)
}

func fireDueTemplates(ctx context.Context, deps schedulingDeps, log *slog.Logger) {
	templates, err := deps.ListTemplates(ctx)
	if err != nil {
		log.Warn("scheduling tick: listing templates", "error", err)
		return
	}
	now := time.Now()
	for _, tmpl := range templates {
		if tmpl.Schedule == "" || !tmpl.Enabled {
			continue
		}
		runs, err := deps.ListScheduleRunsByTemplate(ctx, tmpl.ID)
		if err != nil {
			log.Warn("scheduling tick: listing runs for template", "template_id", tmpl.ID, "error", err)
			continue
		}
		var lastRun *store.ScheduleRun
		if len(runs) > 0 {
			lastRun = &runs[0] // newest first, per ListScheduleRunsByTemplate's contract
		}
		if !agent.TemplateDue(tmpl, lastRun, now) {
			continue
		}

		// Defensive only: internal/api's create/update validation is the
		// real enforcement of "scheduling is Claude Code only in v1,"
		// rejecting this combination before a template can ever be saved.
		// This check exists for a record saved before that validation
		// existed, or reached this state some other inconsistent way.
		if tmpl.Harness == config.HarnessOpenCode {
			log.Warn("scheduling tick: template has the opencode harness, which scheduling does not support; skipping", "template_id", tmpl.ID)
			continue
		}

		a, err := deps.CreateFromTemplate(ctx, tmpl)
		if err != nil {
			// Deliberately no ScheduleRun for this attempt: there is no
			// agent to ever check for completion, so a run record here
			// would need its own special-cased "has no AgentID" handling
			// everywhere else a ScheduleRun is read -- not worth it for a
			// failure this log line already surfaces.
			log.Warn("scheduling tick: creating agent from template", "template_id", tmpl.ID, "error", err)
			continue
		}
		if _, err := deps.CreateScheduleRun(ctx, tmpl.ID, a.ID); err != nil {
			log.Warn("scheduling tick: recording schedule run", "template_id", tmpl.ID, "agent_id", a.ID, "error", err)
		}
	}
}

func checkRunningSchedules(ctx context.Context, deps schedulingDeps, log *slog.Logger) {
	runs, err := deps.ListScheduleRunsByStatus(ctx, store.ScheduleRunStatusRunning)
	if err != nil {
		log.Warn("scheduling tick: listing running schedule runs", "error", err)
		return
	}
	for _, run := range runs {
		status, err := deps.CheckScheduledRunStatus(ctx, run)
		if err != nil {
			log.Warn("scheduling tick: checking run status", "run_id", run.ID, "error", err)
			continue
		}

		switch status {
		case "running":
			continue
		case "gone":
			// The agent is gone (deleted out-of-band, or by an earlier,
			// crashed OnComplete pass) -- finalize as an error rather than
			// leaving this run "running" forever, re-checked every tick
			// with nothing ever able to finish it.
			if _, err := deps.FinishScheduleRun(ctx, run.ID, store.ScheduleRunStatusError); err != nil {
				log.Warn("scheduling tick: finalizing a gone run as error", "run_id", run.ID, "error", err)
			}
			continue
		}
		// status == "done"

		// A template deleted while this run was in flight -- allowed, since
		// a template's own lifecycle is independent of both its runs and
		// their agents -- leaves tmpl as the zero value here, whose
		// OnComplete ("") ApplyOnComplete's switch treats as "leave" (its
		// safest no-op default). The run still gets finalized below either
		// way; only the cleanup policy silently becomes "leave" instead of
		// whatever the deleted template specified.
		tmpl, err := deps.GetTemplate(ctx, run.TemplateID)
		if err != nil && !store.IsTemplateNotFound(err) {
			log.Warn("scheduling tick: loading template for completed run", "run_id", run.ID, "template_id", run.TemplateID, "error", err)
			continue
		}
		if err := deps.ApplyOnComplete(ctx, tmpl, run); err != nil {
			log.Warn("scheduling tick: applying on-complete policy", "run_id", run.ID, "error", err)
		}
		if _, err := deps.FinishScheduleRun(ctx, run.ID, store.ScheduleRunStatusSuccess); err != nil {
			log.Warn("scheduling tick: finalizing schedule run", "run_id", run.ID, "error", err)
		}
	}
}
```

- [ ] **Step 4: Run the tests and verify they pass**

```sh
cd /workspace/ai-sandbox/docker-operator
DP=$(cat /workspace/dependaproxy-ip); DIND=$(node -e 'require("dns").lookup("docker",(e,a)=>console.log(a))')
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host="dependaproxy:$DP" --add-host="docker:$DIND" --env-file /workspace/go.env \
  -e HOME=/work/.gocache-docker-operator -e GOCACHE=/work/.gocache-docker-operator/build \
  -e GOMODCACHE=/work/.gocache-docker-operator/mod -e GOBIN=/work/.gocache-docker-operator/bin \
  -e PATH=/work/.gocache-docker-operator/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
  golang:1.26 go test ./cmd/docker-operator/... -run TestRunSchedulingTickOnce -v
```

Expected: all `PASS`.

- [ ] **Step 5: Wire the ticker into `main.go`**

In `cmd/docker-operator/main.go`, after the existing
`stopPeriodicReconcile := startPeriodicReconcile(...)` block (~line 149-150):

```go
	stopSchedulingTicker := startSchedulingTicker(mgr, schedulingTickInterval, log)
	defer stopSchedulingTicker()
```

Add the new interval constant alongside this file's other interval
constants (e.g. near `periodicReconcileInterval`'s own declaration):

```go
// schedulingTickInterval is how often the scheduling ticker checks for due
// templates and completed runs -- roughly cron's own granularity (a minute),
// since nothing finer-grained than a minute is expressible in a standard
// 5-field cron expression anyway.
const schedulingTickInterval = 1 * time.Minute
```

`mgr` (the `*agent.Manager` `main.go` already constructs) satisfies
`schedulingDeps` once Task 2's methods exist on it — no adapter needed,
the same way `mgr` already satisfies `reconciler` for
`startPeriodicReconcile`.

- [ ] **Step 6: Run the full gate**

```sh
cd /workspace/ai-sandbox/docker-operator
DP=$(cat /workspace/dependaproxy-ip); DIND=$(node -e 'require("dns").lookup("docker",(e,a)=>console.log(a))')
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host="dependaproxy:$DP" --add-host="docker:$DIND" --env-file /workspace/go.env \
  -e HOME=/work/.gocache-docker-operator -e GOCACHE=/work/.gocache-docker-operator/build \
  -e GOMODCACHE=/work/.gocache-docker-operator/mod -e GOBIN=/work/.gocache-docker-operator/bin \
  -e PATH=/work/.gocache-docker-operator/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
  golang:1.26 sh -c "gofmt -l . && go vet ./... && go test ./... -race"
```

Expected: `gofmt -l` prints nothing, no vet warnings, all tests `PASS`.

- [ ] **Step 7: Commit**

```bash
cd /workspace/ai-sandbox
git add docker-operator/cmd/docker-operator/schedule.go docker-operator/cmd/docker-operator/schedule_test.go \
  docker-operator/cmd/docker-operator/main.go
git commit -m "$(cat <<'EOF'
feat(cmd): fire due schedules and check run completion on a ticker

One new periodic ticker, modeled on startPeriodicReconcile: each tick
fires every due, enabled Template (creating a fresh agent via
CreateFromTemplate) and checks every in-flight ScheduleRun for
completion, applying the template's stop/delete/leave policy and
finalizing the run. Multiple due templates fire in the same tick;
one template's or one run's failure never blocks the rest.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `internal/api` — validation and run-history route

**Files:**
- Modify: `internal/api/templates.go` (extend `templateRequest`,
  `validateTemplateFields`, `toTemplateCreateSpec`; add
  `handleListScheduleRuns`)
- Modify: `internal/api/handlers.go` (add `ListScheduleRunsByTemplate` to
  the `AgentManager` interface, ~line 132; register the new route, ~line
  213)
- Modify: `internal/api/handlers_test.go` (add `scheduleRuns` to
  `fakeManager` and implement `ListScheduleRunsByTemplate` on it, ~line 86
  and ~line 539)
- Modify: `internal/api/templates_test.go` (the new tests)

**Interfaces:**
- Consumes: `agent.ParseSchedule`, `(m *Manager) ListScheduleRunsByTemplate`
  (both Task 2 — the latter exists on the REAL `*agent.Manager` already by
  this point, added there rather than here because Task 3's
  `schedulingDeps` interface needs it on `Manager` before this task ever
  runs; this task still has to declare it on the `AgentManager` interface
  `internal/api` actually programs against, and implement it on the
  package's own `fakeManager` test double — neither of those exists yet);
  `store.Template`'s five new fields, `store.ScheduleRun` (Task 1);
  `config.ValidHarness`, `config.HarnessOpenCode` (already exist).
- Produces: `GET /api/templates/{id}/runs` → `{"runs": [store.ScheduleRun,
  ...]}`; `AgentManager.ListScheduleRunsByTemplate` (interface method);
  `(*fakeManager) ListScheduleRunsByTemplate` (test double).

- [ ] **Step 1: Write the failing tests**

Add to `internal/api/templates_test.go` (confirmed: this is where
`TestCreateTemplate`/`TestCreateTemplate_MissingName`/
`TestCreateTemplate_InvalidBackend` already live):

```go
func TestValidateTemplateFields_ScheduleRequiresHarnessTaskOnComplete(t *testing.T) {
	req := templateRequest{Name: "x", Schedule: "0 9 * * *"}
	if ae := validateTemplateFields(req); ae == nil {
		t.Error("validateTemplateFields(schedule, no harness/task/on_complete) = nil, want an error")
	}
}

func TestValidateTemplateFields_ScheduleValid(t *testing.T) {
	req := templateRequest{
		Name: "x", Backend: "ollama", Harness: "claude-code",
		Schedule: "0 9 * * *", Task: "do it", OnComplete: "stop",
	}
	if ae := validateTemplateFields(req); ae != nil {
		t.Errorf("validateTemplateFields(valid schedule) = %v, want nil", ae)
	}
}

func TestValidateTemplateFields_MalformedCronRejected(t *testing.T) {
	req := templateRequest{
		Name: "x", Backend: "ollama", Harness: "claude-code",
		Schedule: "not a cron expression", Task: "do it", OnComplete: "stop",
	}
	if ae := validateTemplateFields(req); ae == nil {
		t.Error("validateTemplateFields(malformed cron) = nil, want an error")
	}
}

func TestValidateTemplateFields_UnknownOnCompleteRejected(t *testing.T) {
	req := templateRequest{
		Name: "x", Backend: "ollama", Harness: "claude-code",
		Schedule: "0 9 * * *", Task: "do it", OnComplete: "nuke-it-from-orbit",
	}
	if ae := validateTemplateFields(req); ae == nil {
		t.Error("validateTemplateFields(unknown on_complete) = nil, want an error")
	}
}

func TestValidateTemplateFields_OpencodeWithScheduleRejected(t *testing.T) {
	req := templateRequest{
		Name: "x", Backend: "ollama", Harness: "opencode",
		Schedule: "0 9 * * *", Task: "do it", OnComplete: "stop",
	}
	if ae := validateTemplateFields(req); ae == nil {
		t.Error("validateTemplateFields(opencode + schedule) = nil, want an error (v1 is Claude Code only)")
	}
}

func TestValidateTemplateFields_NoScheduleIgnoresTheRest(t *testing.T) {
	// Every pre-existing template (no Schedule) must still validate exactly
	// as it did before this feature -- no new required fields for it.
	req := templateRequest{Name: "plain template", Backend: "ollama"}
	if ae := validateTemplateFields(req); ae != nil {
		t.Errorf("validateTemplateFields(no schedule) = %v, want nil", ae)
	}
}
```

Add a handler-level test to `internal/api/templates_test.go`, in this
package's own established style (`newFakeManager`/`newTestHandler`/
`doJSON`, confirmed from its existing `TestCreateTemplate*` tests — never
calling a handler method directly, always through the real mux):

```go
func TestHandleListScheduleRuns(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.scheduleRuns = map[string][]store.ScheduleRun{
		"tpl_1": {{ID: "run_1", TemplateID: "tpl_1", AgentID: "agt_1", Status: store.ScheduleRunStatusSuccess}},
	}
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/templates/tpl_1/runs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var body scheduleRunListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(body.Runs) != 1 || body.Runs[0].ID != "run_1" {
		t.Errorf("Runs = %+v, want [run_1]", body.Runs)
	}
}

func TestHandleListScheduleRuns_EmptyForUnknownTemplate(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/templates/tpl_missing/runs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var body scheduleRunListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Runs == nil {
		t.Error("Runs = nil, want [] (not null) for a template with no runs")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```sh
cd /workspace/ai-sandbox/docker-operator
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host="dependaproxy:$(cat /workspace/dependaproxy-ip)" --env-file /workspace/go.env \
  -e HOME=/work/.gocache-docker-operator -e GOCACHE=/work/.gocache-docker-operator/build \
  -e GOMODCACHE=/work/.gocache-docker-operator/mod -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
  golang:1.26 go test ./internal/api/... -run 'TestValidateTemplateFields|TestHandleListScheduleRuns' -v
```

Expected: compile errors / assertion failures (`unknown field Schedule`,
`undefined: handleListScheduleRuns`) — none of this exists yet.

- [ ] **Step 3: Extend `templateRequest` and its translations**

In `internal/api/templates.go`, add to `templateRequest` (after
`AutoMode`):

```go
	Harness    string `json:"harness"`
	Schedule   string `json:"schedule"`
	Enabled    bool   `json:"enabled"`
	Task       string `json:"task"`
	OnComplete string `json:"on_complete"`
```

Add the same five fields to `toTemplateCreateSpec`'s returned literal:

```go
		Harness:              t.Harness,
		Schedule:             t.Schedule,
		Enabled:              t.Enabled,
		Task:                 t.Task,
		OnComplete:           t.OnComplete,
```

(`toAgentFieldsRequest` is NOT touched: `Harness` here is a
*template-level* field consumed only by the scheduler, never forwarded
into `createAgentRequest`'s own field space the way the 9 shared ones are
— a human applying this template in the create form still picks harness
themselves, exactly as today.)

- [ ] **Step 4: Extend `validateTemplateFields`**

```go
// validOnComplete reports whether s is one of the three values ApplyOnComplete
// understands. Template-only: agents have no OnComplete concept, so this
// lives here rather than in internal/config alongside the agent-level
// validators.
func validOnComplete(s string) bool {
	return s == "stop" || s == "delete" || s == "leave"
}

func validateTemplateFields(req templateRequest) *apiErr {
	if strings.TrimSpace(req.Name) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"name" must not be empty`, "name"}
	}
	if ae := validateAgentFields(req.toAgentFieldsRequest()); ae != nil {
		return ae
	}
	if req.Schedule == "" {
		return nil
	}
	if req.Harness == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"harness" is required when "schedule" is set`, "harness"}
	}
	if !config.ValidHarness(req.Harness) {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"harness" must be "claude-code" or "opencode"`, "harness"}
	}
	if req.Harness == config.HarnessOpenCode {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `scheduling is not supported for the "opencode" harness yet`, "harness"}
	}
	if strings.TrimSpace(req.Task) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"task" is required when "schedule" is set`, "task"}
	}
	if !validOnComplete(req.OnComplete) {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"on_complete" must be "stop", "delete" or "leave"`, "on_complete"}
	}
	if _, err := agent.ParseSchedule(req.Schedule); err != nil {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"schedule" must be a valid standard 5-field cron expression`, "schedule"}
	}
	return nil
}
```

Add both
`"github.com/psenna/ai-sandbox/docker-operator/internal/config"` and
`"github.com/psenna/ai-sandbox/docker-operator/internal/agent"` to this
file's import block. Confirmed: `internal/api/templates.go`'s current
imports are only `"net/http"`, `"strings"`, and this package's own
`store` — neither `config` nor `agent` is already there (Go imports are
per-file, not per-package; `validateAgentFields`'s own use of `config`
lives in the separate `internal/api/handlers.go`, which does not help
this file).

- [ ] **Step 5: Add `ListScheduleRunsByTemplate` to `AgentManager` and the run-history handler**

`(m *Manager) ListScheduleRunsByTemplate` already exists on the REAL
`*agent.Manager` by this point — Task 2 added it, since
`cmd/docker-operator`'s `schedulingDeps` interface (Task 3) needs it on
`Manager` before Task 4 ever runs. But `internal/api`'s handlers call
`h.mgr`, which is typed as the `AgentManager` interface
(`internal/api/handlers.go`), not `*agent.Manager` directly — so the
interface itself still needs this method declared, or `h.mgr.
ListScheduleRunsByTemplate` will not compile even though the real Manager
already has it.

In `internal/api/handlers.go`, add to the `AgentManager` interface
(next to the other `ListTemplates`/`CreateTemplate`/`UpdateTemplate`/
`DeleteTemplate` declarations, ~line 129-132):

```go
	ListScheduleRunsByTemplate(ctx context.Context, templateID string) ([]store.ScheduleRun, error)
```

`internal/api`'s own tests drive every handler through this interface
with an in-memory `fakeManager` (`internal/api/handlers_test.go`), never
the real `*agent.Manager` — so `fakeManager` needs its own
implementation too. Add a field to `fakeManager`'s struct (next to its
existing `templates map[string]store.Template`, ~line 86):

```go
	scheduleRuns map[string][]store.ScheduleRun
```

And an implementation (next to `fakeManager`'s other `*Template*`
methods):

```go
// ListScheduleRunsByTemplate mirrors ListTemplates's style: a nil map
// entry (the common case -- a template with no scheduled runs yet)
// reads as an empty, never-nil slice, matching the real Store's own
// never-nil-list convention.
func (f *fakeManager) ListScheduleRunsByTemplate(_ context.Context, templateID string) ([]store.ScheduleRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	runs := f.scheduleRuns[templateID]
	out := make([]store.ScheduleRun, len(runs))
	copy(out, runs)
	return out, nil
}
```

In `internal/api/templates.go`:

```go
// scheduleRunListResponse is the GET /api/templates/{id}/runs body, same
// wrapped-array convention as templateListResponse.
type scheduleRunListResponse struct {
	Runs []store.ScheduleRun `json:"runs"`
}

func (h *Handler) handleListScheduleRuns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	runs, err := h.mgr.ListScheduleRunsByTemplate(r.Context(), id)
	if err != nil {
		h.internalError(w, "listing schedule runs for template "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, scheduleRunListResponse{Runs: runs})
}
```

In `internal/api/handlers.go`, register the route next to the existing
template routes:

```go
	mux.HandleFunc("GET /api/templates/{id}/runs", h.handleListScheduleRuns)
```

And add the method-not-allowed fallback alongside `/api/templates/{id}`'s
own:

```go
	mux.HandleFunc("/api/templates/{id}/runs", methodNotAllowed)
```

- [ ] **Step 6: Run the tests and verify they pass**

```sh
cd /workspace/ai-sandbox/docker-operator
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host="dependaproxy:$(cat /workspace/dependaproxy-ip)" --env-file /workspace/go.env \
  -e HOME=/work/.gocache-docker-operator -e GOCACHE=/work/.gocache-docker-operator/build \
  -e GOMODCACHE=/work/.gocache-docker-operator/mod -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
  golang:1.26 sh -c "gofmt -l . && go vet ./... && go test ./internal/api/... ./internal/agent/... -race"
```

Expected: `gofmt -l` prints nothing, no vet warnings, all `PASS`.

- [ ] **Step 7: Commit**

```bash
cd /workspace/ai-sandbox
git add docker-operator/internal/api/templates.go docker-operator/internal/api/handlers.go \
  docker-operator/internal/api/handlers_test.go docker-operator/internal/api/templates_test.go
git commit -m "$(cat <<'EOF'
feat(api): validate scheduled-template fields, expose run history

A Template's Schedule being set now requires Harness, Task and
OnComplete together, rejects malformed cron strings (parsed with the
exact parser the ticker itself uses), and rejects the opencode
harness (v1 is Claude Code only). A template with no Schedule
validates exactly as before -- no new requirement for it. New GET
/api/templates/{id}/runs exposes a template's ScheduleRun history.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: `agent/claude-supervisor.sh` — the completion sentinel

**Files:**
- Modify: `docker-operator/agent/claude-supervisor.sh`

**Interfaces:**
- Produces: the file `/workspace/.task-complete` (matching Task 2's
  `taskCompleteSentinelPath` constant exactly), written the moment this
  script is about to exit after a clean (status 0) `claude` run that
  it does NOT restart.
- Consumes: nothing new.

- [ ] **Step 1: Make the change**

In `docker-operator/agent/claude-supervisor.sh`, change:

```sh
echo "claude-supervisor: claude exited 0 (deliberate); not restarting" >&2
```

to:

```sh
echo "claude-supervisor: claude exited 0 (deliberate); not restarting" >&2
# Written only on this clean-exit path, never on the crash-loop give-up
# path a few lines above (which exits non-zero) -- this is how
# internal/agent.CheckScheduledRunStatus tells a scheduled run's
# `claude -p` finishing cleanly apart from the supervisor simply giving up
# on a broken claude. Every agent's script writes this now, not just
# scheduled ones; it is inert (never read by anything) unless a
# ScheduleRun's completion check is specifically polling for it.
touch /workspace/.task-complete
```

Update this script's own header comment (the block at the top describing
what it does) to mention this one-line addition, right after the existing
paragraph about the clean-exit (`status 0`) case — one sentence is enough,
pointing at the same explanation given inline above rather than
duplicating it.

- [ ] **Step 2: Sync the committed copy**

Per `docker-operator/CLAUDE.md`'s rule: `agent/claude-supervisor.sh` is
baked into the agent image at build time (via the Dockerfile's `COPY`),
not a `go:embed` committed-copy situation like `web/*` — confirm this by
checking `docker-operator/agent/Dockerfile` (or wherever the agent image's
Dockerfile lives) for how it picks up this script; if it is a plain
`COPY agent/claude-supervisor.sh ...` with no separate embedded copy
elsewhere in the tree, there is nothing further to sync. (This differs
from `web/*`'s `make sync-web-embed` requirement — do not run that target
for this file; it would be a no-op for this specific file and is only
needed for Task 6's web changes.)

- [ ] **Step 3: Manual verification**

This script has no existing automated test harness in this repo (confirmed:
no `.bats` or shell-test files under `docker-operator/agent/`). Verify
manually: build the agent image, run a container from it with `claude -p
"echo hello and exit"` as the first invocation, and confirm
`/workspace/.task-complete` exists afterward and the pane is left dead (not
restarted). Do this once, by hand, during this task — it is this task's
only verification, there being no automated harness to add one to without
scope creep well beyond what this feature needs.

- [ ] **Step 4: Commit**

```bash
cd /workspace/ai-sandbox
git add docker-operator/agent/claude-supervisor.sh
git commit -m "$(cat <<'EOF'
feat(agent-image): write a completion sentinel on a clean claude exit

One line, on the clean-exit path only (never the crash-loop give-up
path): touch /workspace/.task-complete right before claude-supervisor
decides not to restart claude. This is the free, unambiguous "this
one-shot task finished" signal internal/agent.CheckScheduledRunStatus
polls for -- inert for every non-scheduled agent, which never has
anything looking at this path.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Web UI — the Templates page and the shrunk template bar

**Files:**
- Modify: `docker-operator/web/render.js` (add `renderTemplatesPage`,
  `renderTemplateForm`, `renderScheduleRunList`; shrink
  `renderTemplateBar` to a pure picker, ~line 451)
- Create: `docker-operator/web/templates.js`
- Modify: `docker-operator/web/app.js` (shrink the template-bar wiring
  that currently handles save/delete; add the new sidebar button wiring)
- Modify: `docker-operator/web/index.html` (add the `templates-btn`
  sidebar button and the `templates.js` script tag)
- Modify: `docker-operator/web/render.test.js` (cover the new/changed
  markup)
- Run `make sync-web-embed` as this task's last step (per
  `docker-operator/CLAUDE.md`)

**Interfaces:**
- Consumes: `GET /api/templates`, `POST /api/templates`, `PUT
  /api/templates/{id}`, `DELETE /api/templates/{id}` (already exist; the
  request/response bodies now carry the five new fields per Task 4), `GET
  /api/templates/{id}/runs` (Task 4).
- Produces: `window.renderTemplatesPage(container)` (same
  dual-export/guard shape as `window.renderFileBrowser`); pure markup
  functions in `render.js`: `renderTemplatesPage(templates)`,
  `renderTemplateForm(template)`, `renderScheduleRunList(runs)`.

- [ ] **Step 1: Write the failing render tests**

Add to `web/render.test.js` (Node's built-in `node:test` + `assert`, this
file's existing style):

```js
test('renderTemplatesPage lists templates with a schedule summary', () => {
	const html = Render.renderTemplatesPage([
		{ id: 'tpl_1', name: 'Daily report', schedule: '0 9 * * *', enabled: true },
		{ id: 'tpl_2', name: 'Plain template', schedule: '', enabled: false },
	]);
	assert.match(html, /Daily report/);
	assert.match(html, /Plain template/);
	assert.match(html, /0 9 \* \* \*/);
});

test('renderTemplateForm includes the five new scheduling fields', () => {
	const html = Render.renderTemplateForm({ id: 'tpl_1', name: 'x' });
	assert.match(html, /name="harness"/);
	assert.match(html, /name="schedule"/);
	assert.match(html, /name="enabled"/);
	assert.match(html, /name="task"/);
	assert.match(html, /name="on_complete"/);
});

test('renderScheduleRunList renders status and timestamps, newest first', () => {
	const html = Render.renderScheduleRunList([
		{ id: 'run_2', agent_id: 'agt_2', status: 'running', started_at: '2026-01-02T09:00:00Z' },
		{ id: 'run_1', agent_id: 'agt_1', status: 'success', started_at: '2026-01-01T09:00:00Z' },
	]);
	assert.match(html, /running/);
	assert.match(html, /success/);
});

test('renderTemplateBar is a pure picker now (no save/delete controls)', () => {
	const html = Render.renderTemplateBar([{ id: 'tpl_1', name: 'x' }]);
	assert.match(html, /template-bar__select/);
	assert.doesNotMatch(html, /template-bar__save/);
	assert.doesNotMatch(html, /template-bar__delete/);
});
```

- [ ] **Step 2: Run the tests to verify they fail**

```sh
cd /workspace/ai-sandbox/docker-operator
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  node:22 node --test web/render.test.js
```

Expected: `renderTemplatesPage`/`renderTemplateForm`/
`renderScheduleRunList` are undefined, and the picker-only test fails
against the current bar (which still has save/delete buttons).

- [ ] **Step 3: Shrink `renderTemplateBar`**

Replace `renderTemplateBar`'s body (`web/render.js`, ~line 451) with a
picker-only version — drop the save/delete buttons and the error
paragraph (errors now surface on the new Templates page, where save/
delete actually happen):

```js
	function renderTemplateBar(templates) {
		var sorted = (templates || []).slice().sort(function (a, b) {
			var an = (a && a.name) || '';
			var bn = (b && b.name) || '';
			return an < bn ? -1 : an > bn ? 1 : 0;
		});
		var options = '<option value="">— Select a template —</option>' +
			sorted.map(function (t) {
				var label = t.name ? escapeHTML(t.name) : '(unnamed template)';
				return '<option value="' + escapeHTML(t.id) + '">' + label + '</option>';
			}).join('');
		return (
			'<div class="template-bar">' +
				'<label class="create-form__row template-bar__row">Template' +
					'<select class="template-bar__select">' + options + '</select>' +
				'</label>' +
			'</div>'
		);
	}
```

- [ ] **Step 4: Add the three new render functions**

Add near `renderTemplateBar` in `web/render.js`:

```js
	// scheduleSummary renders a human-readable one-liner for a template's
	// cron schedule, or '' when it has none -- the Templates list's own
	// cron-to-English isn't ambitious (it shows the raw expression plus
	// enabled/disabled), since a full English-cron translator is out of
	// scope for this feature.
	function scheduleSummary(t) {
		if (!t || !t.schedule) return '';
		return escapeHTML(t.schedule) + (t.enabled ? ' (enabled)' : ' (paused)');
	}

	function renderTemplatesPage(templates) {
		var rows = (templates || []).slice().sort(function (a, b) {
			var an = (a && a.name) || '';
			var bn = (b && b.name) || '';
			return an < bn ? -1 : an > bn ? 1 : 0;
		}).map(function (t) {
			var summary = scheduleSummary(t);
			return (
				'<li class="templates-page__row" data-template-id="' + escapeHTML(t.id) + '">' +
					'<span class="templates-page__name">' + escapeHTML(t.name || '(unnamed template)') + '</span>' +
					(summary ? '<span class="templates-page__schedule">' + summary + '</span>' : '') +
					'<button class="templates-page__edit btn btn--ghost btn--sm" type="button">Edit</button>' +
					'<button class="templates-page__delete btn btn--danger btn--sm" type="button">Delete</button>' +
				'</li>'
			);
		}).join('');
		return (
			'<div class="templates-page">' +
				'<div class="templates-page__header">' +
					'<h2>Templates</h2>' +
					'<button class="templates-page__new btn btn--primary" type="button">New template</button>' +
				'</div>' +
				'<ul class="templates-page__list">' + rows + '</ul>' +
				'<div class="templates-page__detail" hidden></div>' +
				'<p class="templates-page__error" role="alert" hidden></p>' +
			'</div>'
		);
	}

	// renderTemplateForm renders the edit form for one template (or a blank
	// one for "New template" when t is null/undefined) -- every field a
	// Template now carries, the 9 pre-existing agent-config ones plus the 5
	// scheduling ones this feature adds.
	function renderTemplateForm(t) {
		t = t || {};
		return (
			'<form class="template-form">' +
				'<label class="template-form__row">Name' +
					'<input type="text" name="name" value="' + escapeHTML(t.name || '') + '" required>' +
				'</label>' +
				'<label class="template-form__row">Description' +
					'<textarea name="description">' + escapeHTML(t.description || '') + '</textarea>' +
				'</label>' +
				'<label class="template-form__row">Backend' +
					'<input type="text" name="backend" value="' + escapeHTML(t.backend || '') + '">' +
				'</label>' +
				'<label class="template-form__row">Harness' +
					'<select name="harness">' +
						'<option value="">—</option>' +
						'<option value="claude-code"' + (t.harness === 'claude-code' ? ' selected' : '') + '>claude-code</option>' +
						'<option value="opencode"' + (t.harness === 'opencode' ? ' selected' : '') + '>opencode</option>' +
					'</select>' +
				'</label>' +
				'<label class="template-form__row">Model' +
					'<input type="text" name="model" value="' + escapeHTML(t.model || '') + '">' +
				'</label>' +
				'<label class="template-form__row">Fast model' +
					'<input type="text" name="fast_model" value="' + escapeHTML(t.fast_model || '') + '">' +
				'</label>' +
				'<label class="template-form__row">Ollama URL' +
					'<input type="text" name="ollama_url" value="' + escapeHTML(t.ollama_url || '') + '">' +
				'</label>' +
				'<label class="template-form__row">Repo' +
					'<input type="text" name="repo" value="' + escapeHTML(t.repo || '') + '">' +
				'</label>' +
				'<label class="template-form__row">Auto-compact threshold' +
					'<input type="text" name="auto_compact_threshold" value="' + escapeHTML(t.auto_compact_threshold || '') + '">' +
				'</label>' +
				'<label class="template-form__row">Max context tokens' +
					'<input type="text" name="max_context_tokens" value="' + escapeHTML(t.max_context_tokens || '') + '">' +
				'</label>' +
				'<label class="template-form__row">Image tag' +
					'<input type="text" name="image_tag" value="' + escapeHTML(t.image_tag || '') + '">' +
				'</label>' +
				'<label class="template-form__row">Auto mode' +
					'<input type="text" name="auto_mode" value="' + escapeHTML(t.auto_mode || '') + '">' +
				'</label>' +
				'<fieldset class="template-form__schedule">' +
					'<legend>Scheduling</legend>' +
					'<label class="template-form__row">' +
						'<input type="checkbox" name="enabled"' + (t.enabled ? ' checked' : '') + '> Enabled' +
					'</label>' +
					'<label class="template-form__row">Schedule (cron)' +
						'<input type="text" name="schedule" value="' + escapeHTML(t.schedule || '') + '" placeholder="0 9 * * *">' +
					'</label>' +
					'<label class="template-form__row">Task' +
						'<textarea name="task">' + escapeHTML(t.task || '') + '</textarea>' +
					'</label>' +
					'<label class="template-form__row">On complete' +
						'<select name="on_complete">' +
							'<option value="">—</option>' +
							'<option value="stop"' + (t.on_complete === 'stop' ? ' selected' : '') + '>Stop</option>' +
							'<option value="delete"' + (t.on_complete === 'delete' ? ' selected' : '') + '>Delete</option>' +
							'<option value="leave"' + (t.on_complete === 'leave' ? ' selected' : '') + '>Leave</option>' +
						'</select>' +
					'</label>' +
				'</fieldset>' +
				'<div class="template-form__actions">' +
					'<button class="template-form__save btn btn--primary" type="submit">Save</button>' +
				'</div>' +
			'</form>'
		);
	}

	// renderScheduleRunList renders a template's run history, newest first
	// (the API already returns it in that order -- see
	// ListScheduleRunsByTemplate). Each row links to the live agent (still
	// around for OnComplete "stop"/"leave") or, once it no longer exists,
	// nothing further here -- the Files browser is where an archived
	// transcript lives, and this list does not re-derive that link.
	function renderScheduleRunList(runs) {
		var rows = (runs || []).map(function (r) {
			return (
				'<li class="schedule-run-list__row" data-run-id="' + escapeHTML(r.id) + '" data-agent-id="' + escapeHTML(r.agent_id) + '">' +
					'<span class="schedule-run-list__status schedule-run-list__status--' + escapeHTML(r.status) + '">' + escapeHTML(r.status) + '</span>' +
					'<span class="schedule-run-list__started">' + escapeHTML(r.started_at || '') + '</span>' +
					'<span class="schedule-run-list__finished">' + escapeHTML(r.finished_at || '—') + '</span>' +
				'</li>'
			);
		}).join('');
		return '<ul class="schedule-run-list">' + rows + '</ul>';
	}
```

Add the three new functions to the `Render` object literal near the
bottom of the file (confirmed: not a literal `window.Render = {...}`
assignment — it is a plain object named `Render`, later exported via
`global.Render = Render` inside the file's closing IIFE). Add three new
entries right next to the existing `renderTemplateBar: renderTemplateBar,`
line:

```js
		renderTemplatesPage: renderTemplatesPage,
		renderTemplateForm: renderTemplateForm,
		renderScheduleRunList: renderScheduleRunList,
```

- [ ] **Step 5: Run the render tests and verify they pass**

```sh
cd /workspace/ai-sandbox/docker-operator
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  node:22 node --test web/render.test.js
```

Expected: all `PASS`.

- [ ] **Step 6: Create `web/templates.js`**

Mirroring `files.js`'s exact shape (`doFetch`/`apiError`/`fetchJSON`
helpers, a `render(container)` closure, `bind()` re-wiring after every
re-render):

```js
// templates.js -- DOM wiring for the Templates page. Mirrors files.js's
// dual-export/guard shape: pure rendering lives in render.js
// (renderTemplatesPage / renderTemplateForm / renderScheduleRunList,
// unit-tested), this file is the event wiring reviewed by hand.
//
// Exposes window.renderTemplatesPage(container), which app.js calls when
// the sidebar "Templates" button is clicked.
(function () {
	'use strict';

	function doFetch(url, options) {
		return ((typeof window !== 'undefined' && window.OperatorAuth && window.OperatorAuth.fetch) || fetch)(url, options);
	}

	function apiError(status, body) {
		try {
			var env = JSON.parse(body);
			if (env && env.error && env.error.message) return new Error(env.error.message);
		} catch (e) { /* not JSON */ }
		return new Error('request failed (' + status + ')');
	}

	async function fetchJSON(url, options) {
		var resp = await doFetch(url, options);
		var text = await resp.text();
		if (!resp.ok) throw apiError(resp.status, text);
		return text ? JSON.parse(text) : null;
	}

	function formToRequest(form) {
		var fd = new FormData(form);
		return {
			name: fd.get('name') || '',
			description: fd.get('description') || '',
			backend: fd.get('backend') || '',
			harness: fd.get('harness') || '',
			model: fd.get('model') || '',
			fast_model: fd.get('fast_model') || '',
			ollama_url: fd.get('ollama_url') || '',
			repo: fd.get('repo') || '',
			auto_compact_threshold: fd.get('auto_compact_threshold') || '',
			max_context_tokens: fd.get('max_context_tokens') || '',
			image_tag: fd.get('image_tag') || '',
			auto_mode: fd.get('auto_mode') || '',
			enabled: fd.get('enabled') === 'on',
			schedule: fd.get('schedule') || '',
			task: fd.get('task') || '',
			on_complete: fd.get('on_complete') || '',
		};
	}

	function render(container) {
		var state = { templates: [] };

		function showError(msg) {
			var el = container.querySelector('.templates-page__error');
			if (!el) return;
			el.textContent = msg;
			el.hidden = !msg;
		}

		function showDetail(html) {
			var el = container.querySelector('.templates-page__detail');
			if (!el) return;
			el.innerHTML = html;
			el.hidden = !html;
		}

		function load() {
			fetchJSON('/api/templates')
				.then(function (data) {
					state.templates = data.templates || [];
					container.innerHTML = window.Render.renderTemplatesPage(state.templates);
					bind();
				})
				.catch(function (e) {
					container.innerHTML = window.Render.renderTemplatesPage([]);
					bind();
					showError('Could not load templates: ' + e.message);
				});
		}

		function openForm(template) {
			showDetail(window.Render.renderTemplateForm(template));
			if (template && template.id) loadRuns(template.id);
			bindDetail(template);
		}

		function loadRuns(templateID) {
			fetchJSON('/api/templates/' + encodeURIComponent(templateID) + '/runs')
				.then(function (data) {
					var detail = container.querySelector('.templates-page__detail');
					if (!detail) return;
					detail.insertAdjacentHTML('beforeend', window.Render.renderScheduleRunList(data.runs || []));
				})
				.catch(function () { /* run history is best-effort for this view */ });
		}

		function bindDetail(template) {
			var form = container.querySelector('.template-form');
			if (!form) return;
			form.addEventListener('submit', function (ev) {
				ev.preventDefault();
				var body = JSON.stringify(formToRequest(form));
				var req = template && template.id
					? fetchJSON('/api/templates/' + encodeURIComponent(template.id), { method: 'PUT', body: body, headers: { 'Content-Type': 'application/json' } })
					: fetchJSON('/api/templates', { method: 'POST', body: body, headers: { 'Content-Type': 'application/json' } });
				req.then(function () { showDetail(''); load(); })
					.catch(function (e) { showError('Could not save template: ' + e.message); });
			});
		}

		function bind() {
			var newBtn = container.querySelector('.templates-page__new');
			if (newBtn) newBtn.addEventListener('click', function () { openForm(null); });

			container.querySelectorAll('.templates-page__edit').forEach(function (btn) {
				btn.addEventListener('click', function () {
					var id = btn.closest('[data-template-id]').getAttribute('data-template-id');
					var t = state.templates.find(function (x) { return x.id === id; });
					if (t) openForm(t);
				});
			});

			container.querySelectorAll('.templates-page__delete').forEach(function (btn) {
				btn.addEventListener('click', function () {
					var id = btn.closest('[data-template-id]').getAttribute('data-template-id');
					fetchJSON('/api/templates/' + encodeURIComponent(id), { method: 'DELETE' })
						.then(load)
						.catch(function (e) { showError('Could not delete template: ' + e.message); });
				});
			});
		}

		load();
	}

	window.renderTemplatesPage = render;
})();
```

- [ ] **Step 7: Shrink the template-bar wiring in `app.js`**

In `web/app.js`, find `wireTemplateBar` (the function that wires
`.template-bar__save`/`.template-bar__delete`, ~line 387 per the earlier
read of this file). Remove the save/delete wiring — those controls no
longer exist in the shrunk bar's markup (Step 3) — and leave only the
`<select>` → `applyTemplateToForm` wiring that already exists for picking
a template. The exact lines to delete are whichever ones query
`.template-bar__save` and `.template-bar__delete`; `refreshTemplateBar`'s
call sites (anywhere the bar is re-rendered after a template list change)
can also be simplified if they existed only to reflect a save/delete that
no longer happens here, but do not remove `refreshTemplateBar` itself if
anything else still calls it to pick up a NEWLY created template (one
created via the new Templates page) showing up in the create form's
picker — read the surrounding code before deleting a call site, not just
the function body in isolation.

- [ ] **Step 8: Wire the new sidebar button**

In `web/index.html`, add the button (next to `activity-btn`, same markup
shape):

```html
		<button id="templates-btn" class="templates-btn btn btn--ghost" type="button">Templates</button>
```

Add the script tag (after `files.js`):

```html
<script src="templates.js"></script>
```

In `web/app.js`, declare the element (next to `filesBtn`/`activityBtn`):

```js
	var templatesBtn = document.getElementById('templates-btn');
```

Wire its click handler (next to `filesBtn`'s own, same shape):

```js
	if (templatesBtn) {
		templatesBtn.addEventListener('click', function () {
			if (typeof window.teardownActiveView === 'function') window.teardownActiveView();
			state.selectedID = null;
			renderSidebar();
			if (typeof window.renderTemplatesPage === 'function') {
				window.renderTemplatesPage(mainArea);
			}
		});
	}
```

- [ ] **Step 9: Sync the embedded web copy**

```sh
cd /workspace/ai-sandbox/docker-operator
make sync-web-embed
```

Expected: `internal/webui/web/*` picks up every change from `web/*`
(minus `*.test.js`, which `sync-web-embed` deletes from the copy).

- [ ] **Step 10: Run the full web test suite and the embed check**

```sh
cd /workspace/ai-sandbox/docker-operator
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  node:22 node --test web/render.test.js web/terminal.test.js web/auth.test.js web/files.test.js
```

```sh
cd /workspace/ai-sandbox/docker-operator
DP=$(cat /workspace/dependaproxy-ip); DIND=$(node -e 'require("dns").lookup("docker",(e,a)=>console.log(a))')
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator \
  --add-host="dependaproxy:$DP" --add-host="docker:$DIND" --env-file /workspace/go.env \
  -e HOME=/work/.gocache-docker-operator -e GOCACHE=/work/.gocache-docker-operator/build \
  -e GOMODCACHE=/work/.gocache-docker-operator/mod -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
  golang:1.26 sh -c "go vet ./... && gofmt -l . && go test ./... -race"
```

Expected: every JS test `PASS`, the full Go gate green too (nothing in
this task touches Go, but re-running it here confirms Task 1-5's work is
still intact alongside this one).

- [ ] **Step 11: Manual browser verification**

No automated test can confirm the page actually renders and the full
create/edit/delete/schedule-run-history flow works end-to-end in a real
browser. Start the stack, open the web UI, click "Templates," create a
scheduled template, and confirm: the form saves, the list shows the
schedule summary, editing round-trips every field, and (once a trigger has
actually fired, which needs real wall-clock time to pass — not required
before merging this task, just before calling the WHOLE feature verified)
the run-history list populates.

- [ ] **Step 12: Commit**

```bash
cd /workspace/ai-sandbox
git add docker-operator/web/render.js docker-operator/web/render.test.js \
  docker-operator/web/templates.js docker-operator/web/app.js docker-operator/web/index.html \
  docker-operator/internal/webui/web
git commit -m "$(cat <<'EOF'
feat(web): add the Templates page, shrink the template bar to a picker

New sidebar entry, Templates, following the Files/Activity/Settings
pattern: lists every template with its schedule summary, and a form
covering all 9 pre-existing agent-config fields plus the 5 new
scheduling ones, with a template's run history shown alongside it.
The create-form's template bar drops its save/delete controls, since
a template's full definition no longer fits what that form captures.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```
