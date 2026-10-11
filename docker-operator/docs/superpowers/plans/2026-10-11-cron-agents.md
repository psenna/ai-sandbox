# CronAgent/Template separation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split the scheduling fields #233 added directly onto `store.Template` into a new `store.CronAgent` entity, so one reusable Template can back any number of differently-scheduled CronAgents, with a new "Cron Agents" sidebar page/button.

**Architecture:** `Template` reverts to pure agent-creation config (keeps `Harness`, loses `Schedule`/`Enabled`/`Task`/`OnComplete`). A new `CronAgent` (`Name`, `TemplateID`, `Schedule`, `Enabled`, `Task`, `OnComplete`) owns the recurring-job definition and references a `Template` by ID. `ScheduleRun.TemplateID` is renamed `CronAgentID`. The scheduling ticker iterates `CronAgent`s instead of `Template`s, resolving each one's `Template` only to read `Backend`/`Model`/.../`Harness` at creation time. `DeleteTemplate` is blocked (409) while any `CronAgent` still references it. No data migration (nothing in production yet — #233 only just merged).

**Tech Stack:** Go 1.x, bbolt (embedded KV store), `net/http` + `http.ServeMux` (1.22+ method-specific routing), `github.com/robfig/cron/v3`, vanilla JS web UI (no framework), Node's built-in `node:test` for web unit tests.

**Spec:** `docs/superpowers/specs/2026-10-11-cron-agents-design.md`

## Global Constraints

- No data migration: `Template.Schedule`/`Enabled`/`Task`/`OnComplete` and `ScheduleRun.TemplateID` are removed/renamed outright, not migrated (spec decision 3).
- `Harness` stays on `Template`, not duplicated onto `CronAgent` (spec decision 2).
- `CronAgent.Name` becomes the created agent's `store.Agent.Name` too (spec decision 4) — set via `CreateRequest.Name`.
- Deleting a `Template` referenced by any `CronAgent` is rejected with 409 (spec decision 5).
- Naming: `store.CronAgent`, routes under `/api/cron-agents`, sidebar button/page titled "Cron Agents" (spec decision 6).
- Every `CronAgent`-related `Manager` pass-through (`CreateCronAgent`/`GetCronAgent`/`ListCronAgents`/`UpdateCronAgent`/`DeleteCronAgent`) is added in the SAME task as `CreateFromCronAgent`/`CheckScheduledRunStatus`/`ApplyOnComplete` (Task 2 below) — not split across tasks by which package calls it first. This is the exact ordering mistake #233's own plan made and had to self-correct mid-implementation when `go vet` caught three missed pass-throughs.
- Before finishing any task that touches `web/`, run the `sync-web-embed` steps (Task 6) — `make` is unavailable on some containers in this environment, so replicate its exact shell logic by hand and diff-verify (see Task 6).
- This repo's `docker-operator` module has no local `make`/`go` on the controller container in this environment — run Go commands inside a Docker container against `DOCKER_HOST=tcp://docker:2375` per this project's `use-docker` skill, and web tests inside a `node:22` container per the Verification commands below.

## Review Focus

- **Deleting a Template still referenced by an enabled, actively-firing CronAgent must be rejected atomically** — a reviewer should expect `DeleteTemplate` to see a consistent snapshot of the `cron_agents` bucket inside the same bbolt transaction as the delete itself, not a separate read then a racy write. Task 1's `DeleteTemplate` test asserts the 409 happens even with a CronAgent record present, and the implementation does the check and the delete in one `db.Update` transaction.
- **A CronAgent whose Template was deleted out from under it mid-flight must still finalize its in-flight run, not hang "running" forever** — mirrors #233's own "deleted template still finalizes run" fix, now one level removed (a CronAgent surviving its Template's deletion, since nothing blocks that — only the reverse is blocked). Task 3's ticker test pins this for a CronAgent whose own record is deleted mid-flight (the real failure mode now, since Template deletion is blocked while any CronAgent references it).
- **`validateCronAgentFields` must reject a `template_id` that does not reference an existing Template**, not merely a non-empty string — a typo'd or already-deleted ID would otherwise create a CronAgent that fails at every single tick with an opaque 404 buried in the operator's log. Task 4 adds a lookup-based check, not just a non-empty check.
- **The opencode-harness-rejection must follow the Template reference, not a field on CronAgent itself** — since `Harness` no longer exists on CronAgent at all, both `validateCronAgentFields` (API-level) and the ticker's defensive check (Task 3) must look at the *referenced Template's* `Harness`, not assume a field that no longer exists.
- **A pre-existing Template record (saved under #233, with `schedule`/`enabled`/`task`/`on_complete` keys still in its raw JSON) must decode cleanly once those fields are removed from the Go struct**, with the extra JSON keys simply ignored — this is the mirror image of #233's own "pre-schedule record decodes inert" test, and the risk is a decode error or a silent data-loss panic on an old record, not an ignored field.

---

### Task 1: Store layer — revert Template, add CronAgent, rename ScheduleRun field

**Files:**
- Modify: `internal/store/template.go`
- Modify: `internal/store/schedulerun.go`
- Modify: `internal/store/store.go:413` (bucket list)
- Create: `internal/store/cronagent.go`
- Modify: `internal/store/template_test.go`
- Modify: `internal/store/schedulerun_test.go`
- Create: `internal/store/cronagent_test.go`

**Interfaces:**
- Consumes: nothing from other tasks (this is the foundation task).
- Produces: `store.Template` (reverted, no `Schedule`/`Enabled`/`Task`/`OnComplete`), `store.CronAgent` struct + `store.CronAgentCreateSpec` + `store.NewCronAgentID() (string, error)` + `(*Store) CreateCronAgent/GetCronAgent/ListCronAgents/UpdateCronAgent/DeleteCronAgent`, `store.ErrCronAgentNotFound` + `IsCronAgentNotFound(err) bool`, `store.ErrTemplateInUse` + `IsTemplateInUse(err) bool`, `store.ScheduleRun.CronAgentID` (renamed from `TemplateID`), `(*Store) CreateScheduleRun(ctx, cronAgentID, agentID)` (param renamed, same position/type), `(*Store) ListScheduleRunsByCronAgent(ctx, cronAgentID)` (renamed from `ListScheduleRunsByTemplate`). Tasks 2-4 depend on every one of these exact names.

- [ ] **Step 1: Write the failing store tests for the Template revert**

Replace `TestTemplate_ScheduleFieldsRoundTrip` (now testing fields that no longer exist) and rewrite `TestTemplate_PreScheduleRecordDecodesInert` in `internal/store/template_test.go` — delete the first, replace the second with:

```go
// This test pins the Review Focus item: a template saved under #233, with
// schedule/enabled/task/on_complete keys still in its raw JSON, must
// decode cleanly once this feature removes those fields from the Go
// struct -- the extra JSON keys are simply ignored by encoding/json, the
// mirror image of #233's own "pre-schedule record decodes inert" test.
func TestTemplate_LegacyFieldsDecodeInert(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	err := st.updateTemplates(ctx, func(b *bbolt.Bucket) error {
		raw := []byte(`{"id":"tpl_old1","name":"old template","harness":"claude-code",` +
			`"schedule":"0 9 * * *","enabled":true,"task":"do the thing","on_complete":"stop",` +
			`"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)
		return b.Put([]byte("tpl_old1"), raw)
	})
	if err != nil {
		t.Fatalf("seeding a legacy schedule-bearing record: %v", err)
	}

	got, err := st.GetTemplate(ctx, "tpl_old1")
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if got.ID != "tpl_old1" || got.Name != "old template" || got.Harness != "claude-code" {
		t.Errorf("GetTemplate = %+v, want ID/Name/Harness to survive unaffected", got)
	}
}
```

Also update `TestCreateTemplate_InsertsARecord` (unaffected — it never set the scheduling fields) and leave `TestUpdateTemplate_StampsUpdatedAtAndOverwritesFields` as-is unless it sets scheduling fields (check it first — if it does, drop those fields from its `TemplateCreateSpec` literal and its `want` struct literal). Remove any other test in this file that constructs a `TemplateCreateSpec{Schedule: ..., Enabled: ..., Task: ..., OnComplete: ...}` or asserts on those fields; `grep -n "Schedule\|Enabled\|OnComplete\|\.Task\b" internal/store/template_test.go` to find every remaining reference before moving on.

- [ ] **Step 2: Run the store tests to verify they fail to compile**

Run (per this project's `use-docker` skill, mounting `/workspace` and running as uid 1000):
```sh
docker run --rm -u node -v /workspace:/work -w /work/ai-sandbox/docker-operator golang:1.25 \
  go test ./internal/store/... -run TestTemplate_LegacyFieldsDecodeInert -v
```
Expected: compile error or FAIL — `Template` still has the old fields, so nothing is broken yet, but the new test name doesn't exist and the struct literal in `TestTemplate_LegacyFieldsDecodeInert` has no struct-literal dependency (it uses raw JSON) so this specific test should actually PASS already against the CURRENT struct — the real failure signal for this step is any OTHER test in the package that still references `Schedule`/`Enabled`/`Task`/`OnComplete` and will fail to compile once Step 3 removes those fields. Confirm by reading `go vet`'s output after Step 3, not before.

- [ ] **Step 3: Revert the Template struct and TemplateCreateSpec**

In `internal/store/template.go`, remove these four fields (and their doc comments) from the `Template` struct — keep `Harness` exactly as-is:

```go
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

Remove the matching four fields from `TemplateCreateSpec`:

```go
	Schedule   string
	Enabled    bool
	Task       string
	OnComplete string
```

(leave `Harness string` in both).

In `CreateTemplate`, remove these four lines from the `tmpl := Template{...}` literal:
```go
		Schedule:             spec.Schedule,
		Enabled:              spec.Enabled,
		Task:                 spec.Task,
		OnComplete:           spec.OnComplete,
```

Grep `internal/store/template.go` for any other `Schedule`/`Enabled`/`Task`/`OnComplete` reference after this edit — there should be none left (the mutator-based `UpdateTemplate` takes a caller-supplied `func(*Template) error`, so it has no field list of its own to edit).

- [ ] **Step 4: Add the ErrTemplateInUse precondition to DeleteTemplate**

Replace `DeleteTemplate` in `internal/store/template.go`:

```go
// ErrTemplateInUse reports that DeleteTemplate was asked to remove a
// template some CronAgent still references by TemplateID. Checked and
// enforced inside the SAME read-write transaction as the delete itself, so
// there is no window for a CronAgent to be created against a template
// between the check and the delete.
var ErrTemplateInUse = errors.New("template is in use by a cron agent")

// IsTemplateInUse reports whether err was caused by a template still
// referenced by a CronAgent.
func IsTemplateInUse(err error) bool { return errors.Is(err, ErrTemplateInUse) }

// DeleteTemplate removes the template record, unless any CronAgent still
// references it (ErrTemplateInUse) -- a CronAgent silently going stale
// because its config disappeared out from under it is worse than an
// explicit, immediate block the user can act on. Deleting a template that
// is not there is success, matching Delete's idempotent stance for an
// agent; that idempotence and the in-use check are not in tension, since an
// in-use check against a nonexistent template ID simply finds no matching
// CronAgent and the subsequent bucket Delete is already a no-op.
func (s *Store) DeleteTemplate(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		cb, err := s.cronAgentsBucket(tx)
		if err != nil {
			return err
		}
		var inUseBy string
		if err := cb.ForEach(func(k, v []byte) error {
			if inUseBy != "" {
				return nil
			}
			var ca CronAgent
			if err := decodeCronAgent(string(k), v, &ca); err != nil {
				return err
			}
			if ca.TemplateID == id {
				inUseBy = ca.ID
			}
			return nil
		}); err != nil {
			return err
		}
		if inUseBy != "" {
			return fmt.Errorf("deleting template %q: referenced by cron agent %q: %w", id, inUseBy, ErrTemplateInUse)
		}

		tb, err := s.templatesBucket(tx)
		if err != nil {
			return err
		}
		if err := tb.Delete([]byte(id)); err != nil {
			return fmt.Errorf("deleting template %q: %w", id, err)
		}
		return nil
	})
}
```

This references `s.cronAgentsBucket`, `CronAgent`, and `decodeCronAgent`, which Step 6 creates — so Step 4's code will not compile until Step 6 lands. Write it now anyway (this is one task, reviewed as a whole); do not run `go build` until after Step 6.

- [ ] **Step 5: Write the failing tests for DeleteTemplate's referential-integrity check**

Add to `internal/store/template_test.go`:

```go
func TestDeleteTemplate_RejectsWhenReferencedByCronAgent(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()
	tmpl := mustCreateTemplate(t, st, "tpl_referenced", "referenced")
	if _, err := st.CreateCronAgent(ctx, CronAgentCreateSpec{
		ID: "ca_1", Name: "nightly", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Enabled: true, Task: "go", OnComplete: "stop",
	}); err != nil {
		t.Fatalf("CreateCronAgent: %v", err)
	}

	err := st.DeleteTemplate(ctx, tmpl.ID)
	if !IsTemplateInUse(err) {
		t.Fatalf("DeleteTemplate while referenced: err = %v, want IsTemplateInUse", err)
	}

	if _, err := st.GetTemplate(ctx, tmpl.ID); err != nil {
		t.Errorf("GetTemplate after a rejected delete: %v, want the template to still exist", err)
	}
}

func TestDeleteTemplate_SucceedsOnceReferenceRemoved(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()
	tmpl := mustCreateTemplate(t, st, "tpl_referenced", "referenced")
	ca, err := st.CreateCronAgent(ctx, CronAgentCreateSpec{
		ID: "ca_1", Name: "nightly", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Enabled: true, Task: "go", OnComplete: "stop",
	})
	if err != nil {
		t.Fatalf("CreateCronAgent: %v", err)
	}
	if err := st.DeleteCronAgent(ctx, ca.ID); err != nil {
		t.Fatalf("DeleteCronAgent: %v", err)
	}

	if err := st.DeleteTemplate(ctx, tmpl.ID); err != nil {
		t.Fatalf("DeleteTemplate after the reference is gone: %v", err)
	}
}
```

These will not compile yet (`CreateCronAgent`/`CronAgentCreateSpec`/`DeleteCronAgent` do not exist). Proceed to Step 6.

- [ ] **Step 6: Create the CronAgent store entity**

Create `internal/store/cronagent.go`:

```go
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

// ErrCronAgentNotFound reports that no CronAgent with the given ID exists.
// GetCronAgent and UpdateCronAgent wrap it; callers test with
// IsCronAgentNotFound.
var ErrCronAgentNotFound = errors.New("cron agent not found")

// IsCronAgentNotFound reports whether err was caused by a missing CronAgent
// record.
func IsCronAgentNotFound(err error) bool { return errors.Is(err, ErrCronAgentNotFound) }

const (
	cronAgentIDPrefix = "ca_"
	cronAgentIDBytes  = 4
)

// bucketCronAgents holds every saved recurring-job definition, keyed by
// CronAgent ID. Separate from bucketTemplates: the same Template can back
// any number of differently-scheduled CronAgents.
var bucketCronAgents = []byte("cron_agents")

// CronAgent is a recurring job definition: "run this Template's config on
// this schedule, with this task, cleaned up this way." It references a
// Template by ID rather than embedding that config, so one Template can
// back any number of differently-scheduled CronAgents.
type CronAgent struct {
	// ID is the primary key. Immutable: UpdateCronAgent rejects a mutator
	// that changes it.
	ID string `json:"id"`
	// Name identifies this CronAgent in the Cron Agents list AND becomes
	// store.Agent.Name on every agent CreateFromCronAgent creates for it --
	// today a template-created agent has no name at all, so this closes
	// that gap specifically for scheduled agents.
	Name string `json:"name"`
	// TemplateID references the Template whose config (backend, model,
	// harness, ...) this job's agents are created from. DeleteTemplate
	// refuses (ErrTemplateInUse) while any CronAgent still references it.
	TemplateID string `json:"template_id"`
	// Schedule is a standard 5-field cron expression ("0 9 * * *"), parsed
	// with github.com/robfig/cron/v3's ParseStandard.
	Schedule string `json:"schedule"`
	// Enabled gates whether Schedule actually fires.
	Enabled bool `json:"enabled"`
	// Task is the prompt text passed to the harness's one-shot invocation
	// (`claude -p <Task>`) on each triggered run.
	Task string `json:"task"`
	// OnComplete decides what happens to a triggered run's agent once its
	// task finishes: "stop", "delete", or "leave".
	OnComplete string `json:"on_complete"`

	// CreatedAt is when CreateCronAgent inserted the record, in UTC.
	// Immutable.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the record last changed, in UTC. CreateCronAgent
	// sets it equal to CreatedAt, and every UpdateCronAgent stamps it.
	UpdatedAt time.Time `json:"updated_at"`
}

// CronAgentCreateSpec is everything a caller supplies to CreateCronAgent
// (and, as a full-record replacement, to UpdateCronAgent).
type CronAgentCreateSpec struct {
	// ID is required and must be unique. Generate it with NewCronAgentID.
	ID         string
	Name       string
	TemplateID string
	Schedule   string
	Enabled    bool
	Task       string
	OnComplete string
}

// NewCronAgentID returns a fresh CronAgent ID of the form "ca_7f3a9c2d",
// mirroring NewTemplateID/NewID.
func NewCronAgentID() (string, error) {
	var b [cronAgentIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a cron agent id: %w", err)
	}
	return cronAgentIDPrefix + hex.EncodeToString(b[:]), nil
}

// CreateCronAgent inserts a new CronAgent record. It rejects a blank ID and
// a duplicate ID (ErrCronAgentExists). Unlike Template, a CronAgent's Name
// has no uniqueness constraint -- it is a list label, not a selection key.
var ErrCronAgentExists = errors.New("cron agent already exists")

// IsCronAgentExists reports whether err was caused by a duplicate CronAgent
// ID.
func IsCronAgentExists(err error) bool { return errors.Is(err, ErrCronAgentExists) }

func (s *Store) CreateCronAgent(ctx context.Context, spec CronAgentCreateSpec) (CronAgent, error) {
	if spec.ID == "" {
		return CronAgent{}, errors.New("creating a cron agent: the ID must not be empty, generate one with NewCronAgentID")
	}

	now := s.now()
	ca := CronAgent{
		ID:         spec.ID,
		Name:       spec.Name,
		TemplateID: spec.TemplateID,
		Schedule:   spec.Schedule,
		Enabled:    spec.Enabled,
		Task:       spec.Task,
		OnComplete: spec.OnComplete,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	err := s.updateCronAgents(ctx, func(b *bbolt.Bucket) error {
		if b.Get([]byte(spec.ID)) != nil {
			return fmt.Errorf("creating cron agent %q: %w", spec.ID, ErrCronAgentExists)
		}
		return putCronAgent(b, ca)
	})
	if err != nil {
		return CronAgent{}, err
	}
	return ca, nil
}

// GetCronAgent returns the CronAgent with the given ID, or an error
// satisfying IsCronAgentNotFound.
func (s *Store) GetCronAgent(ctx context.Context, id string) (CronAgent, error) {
	var ca CronAgent
	err := s.viewCronAgents(ctx, func(b *bbolt.Bucket) error {
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("getting cron agent %q: %w", id, ErrCronAgentNotFound)
		}
		return decodeCronAgent(id, raw, &ca)
	})
	if err != nil {
		return CronAgent{}, err
	}
	return ca, nil
}

// ListCronAgents returns every CronAgent, sorted by ID because bbolt
// iterates keys in byte order (the web UI sorts by Name for display
// itself). The slice is never nil.
func (s *Store) ListCronAgents(ctx context.Context) ([]CronAgent, error) {
	cronAgents := make([]CronAgent, 0)
	err := s.viewCronAgents(ctx, func(b *bbolt.Bucket) error {
		return b.ForEach(func(k, v []byte) error {
			var ca CronAgent
			if err := decodeCronAgent(string(k), v, &ca); err != nil {
				return err
			}
			cronAgents = append(cronAgents, ca)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing cron agents: %w", err)
	}
	return cronAgents, nil
}

// UpdateCronAgent applies mutate to the CronAgent with the given ID and
// writes the whole record back, mirroring UpdateTemplate's mutator
// pattern. ID and CreatedAt are immutable. UpdatedAt is stamped by this
// method after the mutator runs.
func (s *Store) UpdateCronAgent(ctx context.Context, id string, mutate func(*CronAgent) error) (CronAgent, error) {
	if mutate == nil {
		return CronAgent{}, fmt.Errorf("updating cron agent %q: the mutator must not be nil", id)
	}

	var updated CronAgent
	err := s.updateCronAgents(ctx, func(b *bbolt.Bucket) error {
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("updating cron agent %q: %w", id, ErrCronAgentNotFound)
		}
		var ca CronAgent
		if err := decodeCronAgent(id, raw, &ca); err != nil {
			return err
		}

		before := ca
		if err := mutate(&ca); err != nil {
			return fmt.Errorf("updating cron agent %q: %w", id, err)
		}
		if ca.ID != before.ID {
			return fmt.Errorf("updating cron agent %q: the mutator changed the ID to %q; it is immutable", id, ca.ID)
		}
		if !ca.CreatedAt.Equal(before.CreatedAt) {
			return fmt.Errorf("updating cron agent %q: the mutator changed CreatedAt; it is immutable", id)
		}

		ca.UpdatedAt = s.now()
		if err := putCronAgent(b, ca); err != nil {
			return err
		}
		updated = ca
		return nil
	})
	if err != nil {
		return CronAgent{}, err
	}
	return updated, nil
}

// DeleteCronAgent removes the CronAgent record. Deleting one that is not
// there is success, matching Delete's idempotent stance for an agent. Its
// ScheduleRun history survives (ScheduleRun lives in its own bucket, same
// reasoning as a Template's deletion not cascading its runs).
func (s *Store) DeleteCronAgent(ctx context.Context, id string) error {
	return s.updateCronAgents(ctx, func(b *bbolt.Bucket) error {
		if err := b.Delete([]byte(id)); err != nil {
			return fmt.Errorf("deleting cron agent %q: %w", id, err)
		}
		return nil
	})
}

func (s *Store) viewCronAgents(ctx context.Context, fn func(*bbolt.Bucket) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(func(tx *bbolt.Tx) error {
		b, err := s.cronAgentsBucket(tx)
		if err != nil {
			return err
		}
		return fn(b)
	})
}

func (s *Store) updateCronAgents(ctx context.Context, fn func(*bbolt.Bucket) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := s.cronAgentsBucket(tx)
		if err != nil {
			return err
		}
		return fn(b)
	})
}

// cronAgentsBucket returns the cron_agents bucket. Open creates it, so a
// nil here means the file was truncated or was never this program's
// database.
func (s *Store) cronAgentsBucket(tx *bbolt.Tx) (*bbolt.Bucket, error) {
	b := tx.Bucket(bucketCronAgents)
	if b == nil {
		return nil, fmt.Errorf("the %q bucket is missing from the state database %q", bucketCronAgents, s.path)
	}
	return b, nil
}

func putCronAgent(b *bbolt.Bucket, ca CronAgent) error {
	raw, err := json.Marshal(ca)
	if err != nil {
		return fmt.Errorf("encoding cron agent %q: %w", ca.ID, err)
	}
	if err := b.Put([]byte(ca.ID), raw); err != nil {
		return fmt.Errorf("writing cron agent %q: %w", ca.ID, err)
	}
	return nil
}

func decodeCronAgent(id string, raw []byte, ca *CronAgent) error {
	if err := json.Unmarshal(raw, ca); err != nil {
		return fmt.Errorf("decoding cron agent %q from the state database: %w", id, err)
	}
	return nil
}
```

Add `bucketCronAgents` to the bucket-creation list in `internal/store/store.go:413`:

```go
		for _, name := range [][]byte{bucketAgents, bucketSettings, bucketTemplates, bucketAnthropicAccounts, bucketScheduleRuns, bucketCronAgents} {
```

- [ ] **Step 7: Write the CronAgent CRUD tests**

Create `internal/store/cronagent_test.go`, mirroring `template_test.go`'s own shape:

```go
package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func mustCreateCronAgent(t *testing.T, s *Store, id, name, templateID string) CronAgent {
	t.Helper()
	ca, err := s.CreateCronAgent(context.Background(), CronAgentCreateSpec{
		ID: id, Name: name, TemplateID: templateID, Schedule: "0 9 * * *", Enabled: true, Task: "do the thing", OnComplete: "stop",
	})
	if err != nil {
		t.Fatalf("CreateCronAgent(%q, %q): %v", id, name, err)
	}
	return ca
}

func TestOpen_CreatesTheCronAgentsBucket(t *testing.T) {
	s := newStore(t, 1)
	if _, err := s.ListCronAgents(context.Background()); err != nil {
		t.Fatalf("ListCronAgents on a fresh store: %v, want the bucket to already exist", err)
	}
}

func TestNewCronAgentID_FormatAndUniqueness(t *testing.T) {
	a, err := NewCronAgentID()
	if err != nil {
		t.Fatalf("NewCronAgentID: %v", err)
	}
	if len(a) != len(cronAgentIDPrefix)+2*cronAgentIDBytes {
		t.Errorf("NewCronAgentID() = %q, wrong length", a)
	}
	b, err := NewCronAgentID()
	if err != nil {
		t.Fatalf("NewCronAgentID: %v", err)
	}
	if a == b {
		t.Errorf("NewCronAgentID returned the same ID twice: %q", a)
	}
}

func TestCreateCronAgent_InsertsARecord(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	s.now = clockAt(baseTime, time.Minute)

	got, err := s.CreateCronAgent(ctx, CronAgentCreateSpec{
		ID: "ca_7f3a9c2d", Name: "nightly report", TemplateID: "tpl_1",
		Schedule: "0 9 * * *", Enabled: true, Task: "write today's report", OnComplete: "delete",
	})
	if err != nil {
		t.Fatalf("CreateCronAgent: %v", err)
	}

	want := CronAgent{
		ID: "ca_7f3a9c2d", Name: "nightly report", TemplateID: "tpl_1",
		Schedule: "0 9 * * *", Enabled: true, Task: "write today's report", OnComplete: "delete",
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CreateCronAgent returned %+v, want %+v", got, want)
	}

	stored, err := s.GetCronAgent(ctx, got.ID)
	if err != nil {
		t.Fatalf("GetCronAgent: %v", err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("GetCronAgent returned %+v, want %+v (every field must round-trip through JSON)", stored, want)
	}
}

func TestCreateCronAgent_RejectsAnEmptyID(t *testing.T) {
	_, err := newStore(t, 1).CreateCronAgent(context.Background(), CronAgentCreateSpec{Name: "x", TemplateID: "tpl_1"})
	if err == nil {
		t.Fatal("CreateCronAgent with an empty ID returned nil, want an error")
	}
}

func TestCreateCronAgent_RejectsADuplicateID(t *testing.T) {
	s := newStore(t, 1)
	mustCreateCronAgent(t, s, "ca_00000001", "first", "tpl_1")

	_, err := s.CreateCronAgent(context.Background(), CronAgentCreateSpec{ID: "ca_00000001", Name: "second", TemplateID: "tpl_1"})
	if !IsCronAgentExists(err) {
		t.Fatalf("CreateCronAgent with a duplicate ID: err = %v, want one satisfying IsCronAgentExists", err)
	}
}

func TestGetCronAgent_NotFound(t *testing.T) {
	_, err := newStore(t, 1).GetCronAgent(context.Background(), "ca_missing")
	if !IsCronAgentNotFound(err) {
		t.Fatalf("GetCronAgent(missing): err = %v, want IsCronAgentNotFound", err)
	}
}

func TestListCronAgents_EmptyStoreReturnsANonNilSlice(t *testing.T) {
	got, err := newStore(t, 1).ListCronAgents(context.Background())
	if err != nil {
		t.Fatalf("ListCronAgents: %v", err)
	}
	if got == nil {
		t.Error("ListCronAgents on an empty store returned nil, want a non-nil empty slice")
	}
}

func TestListCronAgents_ReturnsEverySortedByID(t *testing.T) {
	s := newStore(t, 1)
	mustCreateCronAgent(t, s, "ca_2", "two", "tpl_1")
	mustCreateCronAgent(t, s, "ca_1", "one", "tpl_1")

	got, err := s.ListCronAgents(context.Background())
	if err != nil {
		t.Fatalf("ListCronAgents: %v", err)
	}
	if len(got) != 2 || got[0].ID != "ca_1" || got[1].ID != "ca_2" {
		t.Errorf("ListCronAgents = %+v, want [ca_1, ca_2] (byte order)", got)
	}
}

func TestUpdateCronAgent_StampsUpdatedAtAndOverwritesFields(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	s.now = clockAt(baseTime, time.Minute)
	ca := mustCreateCronAgent(t, s, "ca_1", "nightly", "tpl_1")

	updated, err := s.UpdateCronAgent(ctx, ca.ID, func(c *CronAgent) error {
		c.Name = "renamed"
		c.Schedule = "0 10 * * *"
		c.Enabled = false
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateCronAgent: %v", err)
	}
	if updated.Name != "renamed" || updated.Schedule != "0 10 * * *" || updated.Enabled {
		t.Errorf("UpdateCronAgent returned %+v, want the mutated fields applied", updated)
	}
	if !updated.UpdatedAt.After(ca.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want it stamped after the original %v", updated.UpdatedAt, ca.UpdatedAt)
	}
	if !updated.CreatedAt.Equal(ca.CreatedAt) {
		t.Errorf("CreatedAt changed from %v to %v, want it immutable", ca.CreatedAt, updated.CreatedAt)
	}
}

func TestUpdateCronAgent_NotFound(t *testing.T) {
	_, err := newStore(t, 1).UpdateCronAgent(context.Background(), "ca_missing", func(c *CronAgent) error { return nil })
	if !IsCronAgentNotFound(err) {
		t.Fatalf("UpdateCronAgent(missing): err = %v, want IsCronAgentNotFound", err)
	}
}

func TestUpdateCronAgent_RejectsANilMutator(t *testing.T) {
	s := newStore(t, 1)
	ca := mustCreateCronAgent(t, s, "ca_1", "nightly", "tpl_1")
	if _, err := s.UpdateCronAgent(context.Background(), ca.ID, nil); err == nil {
		t.Fatal("UpdateCronAgent(nil mutator) = nil error, want an error")
	}
}

func TestDeleteCronAgent_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	ca := mustCreateCronAgent(t, s, "ca_1", "nightly", "tpl_1")

	if err := s.DeleteCronAgent(ctx, ca.ID); err != nil {
		t.Fatalf("DeleteCronAgent: %v", err)
	}
	if err := s.DeleteCronAgent(ctx, ca.ID); err != nil {
		t.Fatalf("DeleteCronAgent again: %v, want idempotent success", err)
	}
	if _, err := s.GetCronAgent(ctx, ca.ID); !IsCronAgentNotFound(err) {
		t.Errorf("GetCronAgent after delete: err = %v, want IsCronAgentNotFound", err)
	}
}

func TestCronAgentsBucketMissingSurfacesAsAnError(t *testing.T) {
	s := newStore(t, 1)
	if err := s.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket(bucketCronAgents) }); err != nil {
		t.Fatalf("deleting the cron_agents bucket to simulate corruption: %v", err)
	}
	if _, err := s.ListCronAgents(context.Background()); err == nil {
		t.Error("ListCronAgents with a missing bucket = nil error, want an error")
	}
}

func TestCorruptCronAgentRecordSurfacesAsAnError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, 1)
	if err := s.updateCronAgents(ctx, func(b *bbolt.Bucket) error {
		return b.Put([]byte("ca_corrupt"), []byte("not json"))
	}); err != nil {
		t.Fatalf("seeding a corrupt record: %v", err)
	}
	if _, err := s.GetCronAgent(ctx, "ca_corrupt"); err == nil {
		t.Error("GetCronAgent(corrupt record) = nil error, want an error")
	}
}
```

`internal/store/cronagent_test.go` needs `"go.etcd.io/bbolt"` imported too (used by the last two tests) — add it to the import block above.

- [ ] **Step 8: Rename ScheduleRun.TemplateID to CronAgentID**

In `internal/store/schedulerun.go`:
- Rename the struct field: `TemplateID string `json:"template_id"`` → `CronAgentID string `json:"cron_agent_id"`` (this changes the JSON wire format — intentional, no migration per the Global Constraints).
- Rename `CreateScheduleRun(ctx context.Context, templateID, agentID string)` 's parameter to `cronAgentID` and its use inside the literal: `TemplateID: templateID` → `CronAgentID: cronAgentID`.
- Rename `ListScheduleRunsByTemplate` to `ListScheduleRunsByCronAgent`, its parameter `templateID` → `cronAgentID`, and its body's `run.TemplateID == templateID` → `run.CronAgentID == cronAgentID`.
- Update the function's doc comments to say "CronAgent" instead of "Template" wherever they currently do (e.g. the `ScheduleRun` struct's own doc comment, which says "of one triggered run of a scheduled Template" — update to "of one triggered run of a scheduled CronAgent").

- [ ] **Step 9: Update schedulerun_test.go for the rename**

In `internal/store/schedulerun_test.go`, rename every `TemplateID`/`tpl_` reference tied to a `ScheduleRun` to `CronAgentID`/`ca_`:
- `TestScheduleRun_CreateAndFinish`: `st.CreateScheduleRun(ctx, "tpl_1", "agt_1")` → `st.CreateScheduleRun(ctx, "ca_1", "agt_1")`; `run.TemplateID != "tpl_1"` → `run.CronAgentID != "ca_1"`; `got.TemplateID != finished.TemplateID` → `got.CronAgentID != finished.CronAgentID`.
- Rename `TestScheduleRun_ListByTemplateNewestFirst` to `TestScheduleRun_ListByCronAgentNewestFirst`: `"tpl_a"`/`"tpl_b"` → `"ca_a"`/`"ca_b"`; `st.ListScheduleRunsByTemplate(ctx, "tpl_a")` → `st.ListScheduleRunsByCronAgent(ctx, "ca_a")`.
- `TestScheduleRun_ListByStatus`: `"tpl_a"` → `"ca_a"` (cosmetic only, behavior unchanged).
- Rename `TestDeleteTemplate_DoesNotCascadeScheduleRuns`: it still proves a Template's deletion doesn't take `ScheduleRun` history down with it, but a `ScheduleRun`'s `CronAgentID` no longer has anything to do with the Template's own ID in this new world — write it as:

```go
// TestDeleteTemplate_DoesNotCascadeScheduleRuns pins the Review Focus
// item: a ScheduleRun's history is independent of both the CronAgent and
// the Template lifecycle -- deleting a Template with no CronAgent
// referencing it must not touch ScheduleRun records that happen to exist.
func TestDeleteTemplate_DoesNotCascadeScheduleRuns(t *testing.T) {
	st := newStore(t, 1)
	ctx := context.Background()

	tmpl, err := st.CreateTemplate(ctx, TemplateCreateSpec{ID: "tpl_doomed", Name: "doomed"})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	run, err := st.CreateScheduleRun(ctx, "ca_unrelated", "agt_1")
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
	if got.CronAgentID != "ca_unrelated" {
		t.Errorf("surviving run's CronAgentID = %q, want %q", got.CronAgentID, "ca_unrelated")
	}
}
```

- [ ] **Step 10: Run the full store package test suite**

```sh
docker run --rm -u node -v /workspace:/work -w /work/ai-sandbox/docker-operator golang:1.25 \
  go test ./internal/store/... -v
```
Expected: PASS, every test in `internal/store`.

- [ ] **Step 11: Commit**

```bash
git add internal/store/template.go internal/store/template_test.go \
  internal/store/schedulerun.go internal/store/schedulerun_test.go \
  internal/store/cronagent.go internal/store/cronagent_test.go internal/store/store.go
git commit -m "feat(store): split CronAgent out of Template

Template reverts to pure agent-creation config (Schedule/Enabled/Task/
OnComplete removed, Harness kept). A new CronAgent entity owns the
recurring-job definition and references a Template by ID. ScheduleRun's
TemplateID is renamed CronAgentID. DeleteTemplate now refuses (409,
ErrTemplateInUse) while any CronAgent still references it, checked and
enforced inside the same transaction as the delete.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 2: Manager layer — CronAgent pass-throughs, schedule.go rewrite

**Files:**
- Modify: `internal/agent/templates.go`
- Create: `internal/agent/cronagents.go`
- Modify: `internal/agent/schedule.go`
- Modify: `internal/agent/schedule_test.go`

**Interfaces:**
- Consumes: every `store.*` name Task 1 produced (`store.CronAgent`, `store.CronAgentCreateSpec`, `store.NewCronAgentID`, the five store CRUD methods, `store.IsCronAgentNotFound`, `store.ScheduleRun.CronAgentID`, `(*Store).ListScheduleRunsByCronAgent`).
- Produces: `(*Manager) CreateCronAgent/GetCronAgent/ListCronAgents/UpdateCronAgent/DeleteCronAgent` (mirroring the Template pass-throughs), `(*Manager) CreateFromCronAgent(ctx, ca store.CronAgent) (store.Agent, error)`, `(*Manager) ApplyOnComplete(ctx, ca store.CronAgent, run store.ScheduleRun) error` (signature changed — was `(ctx, tmpl store.Template, run store.ScheduleRun)`), `(*Manager) ListScheduleRunsByCronAgent(ctx, cronAgentID string) ([]store.ScheduleRun, error)` (renamed pass-through), `agent.CronAgentDue(ca store.CronAgent, lastRun *store.ScheduleRun, now time.Time) bool` (renamed from `TemplateDue`, which took `store.Template` — see the naming note in Step 4 below). `(*Manager) CheckScheduledRunStatus` and `(*Manager) execOneShot` are UNCHANGED (they only ever touched `run.AgentID`, never the Template/CronAgent). Task 3 depends on every one of these exact names.

- [ ] **Step 1: Revert the Template pass-throughs in templates.go**

In `internal/agent/templates.go`, `UpdateTemplate`'s mutator currently sets `t.Schedule`/`t.Enabled`/`t.Task`/`t.OnComplete` — remove those four lines:

```go
		t.Schedule = spec.Schedule
		t.Enabled = spec.Enabled
		t.Task = spec.Task
		t.OnComplete = spec.OnComplete
```

(leave `t.Harness = spec.Harness` in place). `ListTemplates`/`GetTemplate`/`CreateTemplate`/`DeleteTemplate` are untouched — none of them referenced the removed fields.

- [ ] **Step 2: Write the Manager-level CronAgent pass-throughs**

Create `internal/agent/cronagents.go`, mirroring `templates.go`'s own shape exactly:

```go
package agent

import (
	"context"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// ListCronAgents returns every saved recurring-job definition, in the same
// order store.ListCronAgents does. A thin pass-through, the same way
// ListTemplates is: a CronAgent touches no Docker resource of its own.
func (m *Manager) ListCronAgents(ctx context.Context) ([]store.CronAgent, error) {
	return m.store.ListCronAgents(ctx)
}

// GetCronAgent returns one CronAgent record by ID, or an error satisfying
// store.IsCronAgentNotFound.
func (m *Manager) GetCronAgent(ctx context.Context, id string) (store.CronAgent, error) {
	return m.store.GetCronAgent(ctx, id)
}

// CreateCronAgent generates a fresh CronAgent ID and inserts a new record.
// No Docker resource to provision and nothing to roll back on failure, so
// this is a direct store call plus ID generation, mirroring CreateTemplate.
func (m *Manager) CreateCronAgent(ctx context.Context, spec store.CronAgentCreateSpec) (store.CronAgent, error) {
	id, err := store.NewCronAgentID()
	if err != nil {
		return store.CronAgent{}, err
	}
	spec.ID = id
	return m.store.CreateCronAgent(ctx, spec)
}

// UpdateCronAgent replaces every field of the CronAgent with the given ID
// from spec (a full-record replace, matching the edit form's submit-every-
// field-every-time contract, mirroring UpdateTemplate). spec.ID is
// ignored; the ID in the URL path is authoritative and immutable.
func (m *Manager) UpdateCronAgent(ctx context.Context, id string, spec store.CronAgentCreateSpec) (store.CronAgent, error) {
	return m.store.UpdateCronAgent(ctx, id, func(c *store.CronAgent) error {
		c.Name = spec.Name
		c.TemplateID = spec.TemplateID
		c.Schedule = spec.Schedule
		c.Enabled = spec.Enabled
		c.Task = spec.Task
		c.OnComplete = spec.OnComplete
		return nil
	})
}

// DeleteCronAgent removes the CronAgent record (idempotent).
func (m *Manager) DeleteCronAgent(ctx context.Context, id string) error {
	return m.store.DeleteCronAgent(ctx, id)
}
```

- [ ] **Step 3: Write the failing schedule.go tests (CronAgentDue + CreateFromCronAgent + ApplyOnComplete)**

Rewrite `internal/agent/schedule_test.go`'s `TemplateDue`-named tests to `CronAgentDue`, operating on `store.CronAgent` instead of `store.Template`:

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

func TestCronAgentDue_NeverRun(t *testing.T) {
	ca := store.CronAgent{
		Schedule:  "0 9 * * *",
		Enabled:   true,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	if !CronAgentDue(ca, nil, now) {
		t.Error("CronAgentDue = false, want true (9am fired since CreatedAt at midnight, now is 10am)")
	}
	if CronAgentDue(ca, nil, time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)) {
		t.Error("CronAgentDue = true, want false (9am has not happened yet)")
	}
}

func TestCronAgentDue_AnchorsOnLastRun(t *testing.T) {
	ca := store.CronAgent{Schedule: "0 9 * * *", Enabled: true}
	lastRun := &store.ScheduleRun{StartedAt: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	if CronAgentDue(ca, lastRun, time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)) {
		t.Error("CronAgentDue = true, want false (already ran today)")
	}
	if !CronAgentDue(ca, lastRun, time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)) {
		t.Error("CronAgentDue = false, want true (tomorrow's 9am has arrived)")
	}
}

func TestCronAgentDue_AnchorsOnUpdatedAt(t *testing.T) {
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	ca := store.CronAgent{
		Schedule: "0 9 * * *", Enabled: true,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: now.Add(-time.Minute),
	}
	if CronAgentDue(ca, nil, now) {
		t.Error("CronAgentDue = true, want false (next 9am after UpdatedAt is tomorrow)")
	}
	if !CronAgentDue(ca, nil, time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)) {
		t.Error("CronAgentDue = false, want true (tomorrow's 9am has arrived)")
	}
	stale := &store.ScheduleRun{StartedAt: time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)}
	if CronAgentDue(ca, stale, now) {
		t.Error("CronAgentDue = true with a stale lastRun, want false (UpdatedAt is newer)")
	}
}

func TestCronAgentDue_DisabledNeverFires(t *testing.T) {
	ca := store.CronAgent{Schedule: "0 9 * * *", Enabled: false}
	if CronAgentDue(ca, nil, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("CronAgentDue(disabled) = true, want false regardless of how far in the future now is")
	}
}

func TestCronAgentDue_NoScheduleNeverFires(t *testing.T) {
	ca := store.CronAgent{Schedule: "", Enabled: true}
	if CronAgentDue(ca, nil, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("CronAgentDue(no schedule) = true, want false")
	}
}
```

This file does not yet test `CreateFromCronAgent`/`ApplyOnComplete` directly (neither did the pre-existing `schedule_test.go` test `CreateFromTemplate`/`ApplyOnComplete` — those are exercised end-to-end through `cmd/docker-operator`'s ticker tests in Task 3, which is the existing division of labor in this codebase; preserve it rather than inventing new unit tests this task's spec never asked for).

- [ ] **Step 4: Run to verify the new tests fail to compile**

```sh
docker run --rm -u node -v /workspace:/work -w /work/ai-sandbox/docker-operator golang:1.25 \
  go test ./internal/agent/... -run TestCronAgentDue -v
```
Expected: compile error — `CronAgentDue` does not exist yet.

- [ ] **Step 5: Rewrite schedule.go**

Replace `internal/agent/schedule.go` in full:

```go
package agent

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

const taskCompleteSentinelPath = "/workspace/.task-complete"
const taskFailedSentinelPath = "/workspace/.task-failed"

var scheduledRunCheckCmd = []string{"sh", "-c",
	"if [ -f '" + taskCompleteSentinelPath + "' ]; then echo complete; " +
		"elif [ -f '" + taskFailedSentinelPath + "' ]; then echo failed; fi"}

func ParseSchedule(expr string) (cron.Schedule, error) {
	return cron.ParseStandard(expr)
}

// CronAgentDue reports whether ca is due to fire at now, anchored on
// whichever of ca.CreatedAt, ca.UpdatedAt, or lastRun.StartedAt is most
// recent -- a CronAgent created long ago whose schedule was just edited (or
// re-enabled) waits for its next real cron fire instead of firing on the
// very next tick off a stale CreatedAt. Named for the entity it now
// operates on (CronAgent, not Template, since #233's own TemplateDue took
// a store.Template) -- keeping the old name on a changed parameter type
// would mislead every future reader of this file.
func CronAgentDue(ca store.CronAgent, lastRun *store.ScheduleRun, now time.Time) bool {
	if !ca.Enabled || ca.Schedule == "" {
		return false
	}
	sched, err := ParseSchedule(ca.Schedule)
	if err != nil {
		return false
	}
	anchor := ca.CreatedAt
	if ca.UpdatedAt.After(anchor) {
		anchor = ca.UpdatedAt
	}
	if lastRun != nil && lastRun.StartedAt.After(anchor) {
		anchor = lastRun.StartedAt
	}
	next := sched.Next(anchor)
	return !next.After(now)
}

// CreateFromCronAgent resolves ca.TemplateID to its Template and creates an
// agent from the Template's config, with Name and OneShotTask coming from
// the CronAgent instead -- the two fields that moved off Template (spec
// decision 1 and 4).
func (m *Manager) CreateFromCronAgent(ctx context.Context, ca store.CronAgent) (store.Agent, error) {
	tmpl, err := m.store.GetTemplate(ctx, ca.TemplateID)
	if err != nil {
		return store.Agent{}, err
	}
	return m.Create(ctx, CreateRequest{
		Name: ca.Name, Backend: tmpl.Backend, Harness: tmpl.Harness, Model: tmpl.Model,
		FastModel: tmpl.FastModel, OllamaURL: tmpl.OllamaURL, Repo: tmpl.Repo,
		AutoCompactThreshold: tmpl.AutoCompactThreshold, MaxContextTokens: tmpl.MaxContextTokens,
		ImageTag: tmpl.ImageTag, AutoMode: tmpl.AutoMode, OneShotTask: ca.Task,
	})
}

func (m *Manager) CheckScheduledRunStatus(ctx context.Context, run store.ScheduleRun) (string, error) {
	ref := agentContainerName(run.AgentID)
	out, err := m.execOneShot(ctx, ref, scheduledRunCheckCmd)
	if err != nil {
		c, ierr := m.docker.ContainerInspect(ctx, ref)
		if dockerclient.IsNotFound(ierr) || (ierr == nil && c.State != dockerclient.StateRunning) {
			return "gone", nil
		}
		return "", err
	}
	switch strings.TrimSpace(string(out)) {
	case "complete":
		return "done", nil
	case "failed":
		return "failed", nil
	default:
		return "running", nil
	}
}

// ApplyOnComplete applies ca's completion policy to run's agent. Its body
// is unchanged from #233's own Template-typed version -- it only ever read
// .OnComplete, which now lives on CronAgent instead of Template.
func (m *Manager) ApplyOnComplete(ctx context.Context, ca store.CronAgent, run store.ScheduleRun) error {
	switch ca.OnComplete {
	case "stop":
		if _, err := m.store.Update(ctx, run.AgentID, func(a *store.Agent) error {
			a.Status = store.StatusStopped
			return nil
		}); err != nil {
			return err
		}
		return m.docker.ContainerStop(ctx, agentContainerName(run.AgentID), m.opts.StopTimeout)
	case "delete":
		return m.Delete(ctx, run.AgentID)
	default:
		return nil
	}
}

func (m *Manager) ListScheduleRunsByCronAgent(ctx context.Context, cronAgentID string) ([]store.ScheduleRun, error) {
	return m.store.ListScheduleRunsByCronAgent(ctx, cronAgentID)
}
func (m *Manager) ListScheduleRunsByStatus(ctx context.Context, status string) ([]store.ScheduleRun, error) {
	return m.store.ListScheduleRunsByStatus(ctx, status)
}
func (m *Manager) CreateScheduleRun(ctx context.Context, cronAgentID, agentID string) (store.ScheduleRun, error) {
	return m.store.CreateScheduleRun(ctx, cronAgentID, agentID)
}
func (m *Manager) FinishScheduleRun(ctx context.Context, id, status string) (store.ScheduleRun, error) {
	return m.store.FinishScheduleRun(ctx, id, status)
}

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

Note the deliberate naming deviation from the spec's literal prose here: the spec's "The ticker" section calls this function "TemplateDue" even while describing it as operating on a CronAgent. That is a leftover from the spec's own prose, not a requirement — renaming it to `CronAgentDue` is a plan-level correction (the function's behavior is unchanged, only its name and parameter type), made because a name/parameter mismatch would mislead every future reader of this file exactly the way #233's own pre-flight review flagged similar drift. Every other rename in this task (`CreateFromCronAgent`, `ApplyOnComplete`'s signature, `ListScheduleRunsByCronAgent`) follows the spec's text exactly.

- [ ] **Step 6: Run the agent package tests**

```sh
docker run --rm -u node -v /workspace:/work -w /work/ai-sandbox/docker-operator golang:1.25 \
  go test ./internal/agent/... -v
```
Expected: PASS, every test in `internal/agent`. (This also exercises `go build`'s type-checking across `templates.go`/`cronagents.go`/`schedule.go` together — if `cronagents.go` and `schedule.go` disagree on `store.CronAgent`'s field names, this step is where it surfaces.)

- [ ] **Step 7: Commit**

```bash
git add internal/agent/templates.go internal/agent/cronagents.go \
  internal/agent/schedule.go internal/agent/schedule_test.go
git commit -m "feat(agent): CronAgent manager pass-throughs and schedule.go rewrite

CreateFromTemplate becomes CreateFromCronAgent, resolving the referenced
Template internally and sourcing Name/OneShotTask from the CronAgent.
ApplyOnComplete now takes a CronAgent. TemplateDue is renamed
CronAgentDue to match its new store.CronAgent parameter. Every
CronAgent-related Manager pass-through lands in this same commit,
alongside the ticker-facing methods that need them, per the spec's
explicit ordering note.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 3: Ticker — fireDueCronAgents, checkRunningSchedules, finishScheduledRun

**Files:**
- Modify: `cmd/docker-operator/schedule.go`
- Modify: `cmd/docker-operator/schedule_test.go`

**Interfaces:**
- Consumes: `agent.CronAgentDue`, `(*Manager).CreateFromCronAgent/ApplyOnComplete/ListScheduleRunsByCronAgent/ListCronAgents/GetCronAgent/GetTemplate/CheckScheduledRunStatus/CreateScheduleRun/FinishScheduleRun/ListScheduleRunsByStatus` from Task 2, `store.IsCronAgentNotFound` from Task 1.
- Produces: the rewritten `schedulingDeps` interface and `startSchedulingTicker`/`runSchedulingTickOnce` — unchanged exported surface (`main.go`'s caller is untouched; confirm with `grep -n "startSchedulingTicker" cmd/docker-operator/main.go` before finishing this task — it should need no edit since the function's own signature `(deps schedulingDeps, interval time.Duration, log *slog.Logger) func()` does not change shape).

- [ ] **Step 1: Write the failing ticker tests**

Replace `cmd/docker-operator/schedule_test.go` in full:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// fakeSchedulingDeps is a minimal in-memory stand-in for schedulingDeps,
// recording exactly what runSchedulingTickOnce does without touching a
// real Manager or Store.
type fakeSchedulingDeps struct {
	cronAgents       []store.CronAgent
	templates        map[string]store.Template
	runsByCronAgent  map[string][]store.ScheduleRun
	runningRuns      []store.ScheduleRun
	created          []string          // cron agent IDs CreateFromCronAgent was called for
	statuses         map[string]string // run ID -> "running" | "done" | "failed" | "gone"
	completed        []string          // run IDs ApplyOnComplete was called for
	finished         map[string]string // run ID -> status FinishScheduleRun was called with
	checked          []string          // run IDs CheckScheduledRunStatus was called for
	createErr        map[string]error  // cron agent ID -> error CreateFromCronAgent returns
	recorded         []store.ScheduleRun
}

func (f *fakeSchedulingDeps) ListCronAgents(ctx context.Context) ([]store.CronAgent, error) {
	return f.cronAgents, nil
}
func (f *fakeSchedulingDeps) GetCronAgent(ctx context.Context, id string) (store.CronAgent, error) {
	for _, ca := range f.cronAgents {
		if ca.ID == id {
			return ca, nil
		}
	}
	return store.CronAgent{}, store.ErrCronAgentNotFound
}
func (f *fakeSchedulingDeps) GetTemplate(ctx context.Context, id string) (store.Template, error) {
	if t, ok := f.templates[id]; ok {
		return t, nil
	}
	return store.Template{}, store.ErrTemplateNotFound
}
func (f *fakeSchedulingDeps) ListScheduleRunsByCronAgent(ctx context.Context, cronAgentID string) ([]store.ScheduleRun, error) {
	return f.runsByCronAgent[cronAgentID], nil
}
func (f *fakeSchedulingDeps) ListScheduleRunsByStatus(ctx context.Context, status string) ([]store.ScheduleRun, error) {
	if status == store.ScheduleRunStatusRunning {
		return f.runningRuns, nil
	}
	return nil, nil
}

// CreateScheduleRun also prepends the new run to runsByCronAgent (newest
// first, matching ListScheduleRunsByCronAgent's contract), so a later tick
// sees it as the cron agent's last run -- exactly what CronAgentDue
// anchors on.
func (f *fakeSchedulingDeps) CreateScheduleRun(ctx context.Context, cronAgentID, agentID string) (store.ScheduleRun, error) {
	run := store.ScheduleRun{
		ID: fmt.Sprintf("run_%s_%d", cronAgentID, len(f.recorded)), CronAgentID: cronAgentID, AgentID: agentID,
		StartedAt: time.Now(), Status: store.ScheduleRunStatusRunning,
	}
	f.recorded = append(f.recorded, run)
	if f.runsByCronAgent == nil {
		f.runsByCronAgent = map[string][]store.ScheduleRun{}
	}
	f.runsByCronAgent[cronAgentID] = append([]store.ScheduleRun{run}, f.runsByCronAgent[cronAgentID]...)
	return run, nil
}
func (f *fakeSchedulingDeps) FinishScheduleRun(ctx context.Context, id, status string) (store.ScheduleRun, error) {
	if f.finished == nil {
		f.finished = map[string]string{}
	}
	f.finished[id] = status
	return store.ScheduleRun{ID: id, Status: status}, nil
}
func (f *fakeSchedulingDeps) CreateFromCronAgent(ctx context.Context, ca store.CronAgent) (store.Agent, error) {
	f.created = append(f.created, ca.ID)
	if err := f.createErr[ca.ID]; err != nil {
		return store.Agent{}, err
	}
	return store.Agent{ID: "agt_for_" + ca.ID}, nil
}
func (f *fakeSchedulingDeps) CheckScheduledRunStatus(ctx context.Context, run store.ScheduleRun) (string, error) {
	f.checked = append(f.checked, run.ID)
	return f.statuses[run.ID], nil
}
func (f *fakeSchedulingDeps) ApplyOnComplete(ctx context.Context, ca store.CronAgent, run store.ScheduleRun) error {
	f.completed = append(f.completed, run.ID)
	return nil
}

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRunSchedulingTickOnce_FiresDueCronAgent(t *testing.T) {
	deps := &fakeSchedulingDeps{
		cronAgents: []store.CronAgent{{
			ID: "ca_1", TemplateID: "tpl_1", Enabled: true, Schedule: "0 9 * * *",
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
		templates: map[string]store.Template{"tpl_1": {ID: "tpl_1"}},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.created) != 1 || deps.created[0] != "ca_1" {
		t.Errorf("created = %v, want [ca_1]", deps.created)
	}
}

func TestRunSchedulingTickOnce_SkipsNotDue(t *testing.T) {
	deps := &fakeSchedulingDeps{
		cronAgents: []store.CronAgent{{
			ID: "ca_1", TemplateID: "tpl_1", Enabled: true, Schedule: "0 9 * * *",
			CreatedAt: time.Now().Add(time.Hour),
		}},
		templates: map[string]store.Template{"tpl_1": {ID: "tpl_1"}},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.created) != 0 {
		t.Errorf("created = %v, want none", deps.created)
	}
}

// TestRunSchedulingTickOnce_FiresAllDueInOneTick pins the Review Focus
// item: multiple due schedules in one tick must ALL fire, and one cron
// agent's failure must not block another's.
func TestRunSchedulingTickOnce_FiresAllDueInOneTick(t *testing.T) {
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	deps := &fakeSchedulingDeps{
		cronAgents: []store.CronAgent{
			{ID: "ca_1", TemplateID: "tpl_1", Enabled: true, Schedule: "0 9 * * *", CreatedAt: past},
			{ID: "ca_2", TemplateID: "tpl_1", Enabled: true, Schedule: "0 10 * * *", CreatedAt: past},
		},
		templates: map[string]store.Template{"tpl_1": {ID: "tpl_1"}},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.created) != 2 {
		t.Errorf("created = %v, want both ca_1 and ca_2", deps.created)
	}
}

func TestRunSchedulingTickOnce_ChecksCompletionAndApplies(t *testing.T) {
	deps := &fakeSchedulingDeps{
		runningRuns: []store.ScheduleRun{{ID: "run_1", CronAgentID: "ca_1", AgentID: "agt_1", StartedAt: time.Now()}},
		statuses:    map[string]string{"run_1": "done"},
		cronAgents:  []store.CronAgent{{ID: "ca_1", OnComplete: "stop"}},
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
		runningRuns: []store.ScheduleRun{{ID: "run_1", CronAgentID: "ca_1", AgentID: "agt_1", StartedAt: time.Now()}},
		statuses:    map[string]string{"run_1": "running"},
		cronAgents:  []store.CronAgent{{ID: "ca_1", OnComplete: "stop"}},
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
		runningRuns: []store.ScheduleRun{{ID: "run_1", CronAgentID: "ca_1", AgentID: "agt_1", StartedAt: time.Now()}},
		statuses:    map[string]string{"run_1": "gone"},
		cronAgents:  []store.CronAgent{{ID: "ca_1", OnComplete: "stop"}},
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
// Constraint that a CronAgent whose referenced Template's Harness is
// opencode never runs, even if a record reached that state before
// internal/api's own validation existed to reject it.
func TestRunSchedulingTickOnce_SkipsOpenCodeTemplate(t *testing.T) {
	deps := &fakeSchedulingDeps{
		cronAgents: []store.CronAgent{{
			ID: "ca_1", TemplateID: "tpl_1", Enabled: true, Schedule: "0 9 * * *",
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
		templates: map[string]store.Template{"tpl_1": {ID: "tpl_1", Harness: "opencode"}},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.created) != 0 {
		t.Errorf("created = %v, want none (opencode templates are rejected defensively)", deps.created)
	}
}

// TestRunSchedulingTickOnce_DeletedCronAgentStillFinalizesRun pins the
// Review Focus item: a run whose CronAgent was deleted while it was still
// in flight must still be checked, still get ApplyOnComplete called (with
// the zero-value CronAgent, whose OnComplete "" is "leave"), and still be
// finalized -- never stuck "running" just because its CronAgent is gone.
// This is the real analog of #233's "deleted template still finalizes
// run" fix: Task 1 blocks deleting a TEMPLATE while a CronAgent references
// it, but nothing blocks deleting the CronAgent itself mid-flight.
func TestRunSchedulingTickOnce_DeletedCronAgentStillFinalizesRun(t *testing.T) {
	deps := &fakeSchedulingDeps{
		runningRuns: []store.ScheduleRun{{ID: "run_1", CronAgentID: "ca_deleted", AgentID: "agt_1", StartedAt: time.Now()}},
		statuses:    map[string]string{"run_1": "done"},
		// No cron agents at all: ca_deleted has been removed.
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.completed) != 1 || deps.completed[0] != "run_1" {
		t.Errorf("completed = %v, want [run_1] (ApplyOnComplete still called, with the zero-value cron agent)", deps.completed)
	}
	if deps.finished["run_1"] != store.ScheduleRunStatusSuccess {
		t.Errorf("finished[run_1] = %q, want %q", deps.finished["run_1"], store.ScheduleRunStatusSuccess)
	}
}

// TestRunSchedulingTickOnce_FailedRunAppliesPolicyAndErrors pins Fix 1:
// a one-shot task whose claude exited non-zero still runs the cron
// agent's cleanup policy, but is finalized as an error, not success.
func TestRunSchedulingTickOnce_FailedRunAppliesPolicyAndErrors(t *testing.T) {
	deps := &fakeSchedulingDeps{
		runningRuns: []store.ScheduleRun{{ID: "run_1", CronAgentID: "ca_1", AgentID: "agt_1", StartedAt: time.Now()}},
		statuses:    map[string]string{"run_1": "failed"},
		cronAgents:  []store.CronAgent{{ID: "ca_1", OnComplete: "delete"}},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.completed) != 1 || deps.completed[0] != "run_1" {
		t.Errorf("completed = %v, want [run_1] (OnComplete still applies to a failed run)", deps.completed)
	}
	if deps.finished["run_1"] != store.ScheduleRunStatusError {
		t.Errorf("finished[run_1] = %q, want %q", deps.finished["run_1"], store.ScheduleRunStatusError)
	}
}

// TestRunSchedulingTickOnce_TimesOutStuckRun pins Fix 1's backstop: a run
// in flight longer than maxScheduledRunDuration is treated as failed
// without even checking its status.
func TestRunSchedulingTickOnce_TimesOutStuckRun(t *testing.T) {
	deps := &fakeSchedulingDeps{
		runningRuns: []store.ScheduleRun{{
			ID: "run_1", CronAgentID: "ca_1", AgentID: "agt_1",
			StartedAt: time.Now().Add(-maxScheduledRunDuration - time.Minute),
		}},
		statuses:   map[string]string{"run_1": "running"},
		cronAgents: []store.CronAgent{{ID: "ca_1", OnComplete: "stop"}},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.checked) != 0 {
		t.Errorf("checked = %v, want none (a timed-out run skips the exec check)", deps.checked)
	}
	if len(deps.completed) != 1 || deps.completed[0] != "run_1" {
		t.Errorf("completed = %v, want [run_1] (OnComplete still applies to a timed-out run)", deps.completed)
	}
	if deps.finished["run_1"] != store.ScheduleRunStatusError {
		t.Errorf("finished[run_1] = %q, want %q", deps.finished["run_1"], store.ScheduleRunStatusError)
	}
}

// TestRunSchedulingTickOnce_CreateFailureRecordsErrorRun pins Fix 3: a
// CreateFromCronAgent failure still produces a ScheduleRun finalized as an
// error (visible in run history), and because that run advances
// CronAgentDue's anchor, the very next tick does not retry it.
func TestRunSchedulingTickOnce_CreateFailureRecordsErrorRun(t *testing.T) {
	deps := &fakeSchedulingDeps{
		cronAgents: []store.CronAgent{{
			ID: "ca_1", TemplateID: "tpl_1", Enabled: true, Schedule: "0 9 * * *",
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
		templates: map[string]store.Template{"tpl_1": {ID: "tpl_1"}},
		createErr: map[string]error{"ca_1": errors.New("image pull failed")},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.recorded) != 1 {
		t.Fatalf("recorded runs = %+v, want exactly one for the failed attempt", deps.recorded)
	}
	run := deps.recorded[0]
	if run.CronAgentID != "ca_1" || run.AgentID != "" {
		t.Errorf("recorded run = %+v, want CronAgentID ca_1 and an empty AgentID", run)
	}
	if deps.finished[run.ID] != store.ScheduleRunStatusError {
		t.Errorf("finished[%s] = %q, want %q", run.ID, deps.finished[run.ID], store.ScheduleRunStatusError)
	}

	runSchedulingTickOnce(context.Background(), deps, testLog())
	if len(deps.created) != 1 {
		t.Errorf("created after a second tick = %v, want still just [ca_1] (the anchor advanced, not due again today)", deps.created)
	}
}
```

- [ ] **Step 2: Run to verify the new tests fail to compile**

```sh
docker run --rm -u node -v /workspace:/work -w /work/ai-sandbox/docker-operator golang:1.25 \
  go test ./cmd/docker-operator/... -run TestRunSchedulingTickOnce -v
```
Expected: compile error — `schedulingDeps` still has the old method set, `store.CronAgent`/`store.ErrCronAgentNotFound` referenced by the test file compile fine (Task 1 already added them), but `cmd/docker-operator/schedule.go` has no matching implementation yet.

- [ ] **Step 3: Rewrite schedule.go**

Replace `cmd/docker-operator/schedule.go` in full:

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

// maxScheduledRunDuration is a backstop independent of the completion
// sentinels: a run in flight longer than this is finalized as an error
// (after its OnComplete policy runs), so no failure mode -- a hung claude,
// a sentinel never written -- can leak a run and its agent slot forever.
const maxScheduledRunDuration = 2 * time.Hour

// schedulingDeps is the Manager/Store surface runSchedulingTickOnce needs,
// as an interface seam -- the same role reconciler plays for
// startPeriodicReconcile -- so schedule_test.go can drive it with an
// in-memory fake instead of a real Manager and BoltDB file.
type schedulingDeps interface {
	ListCronAgents(ctx context.Context) ([]store.CronAgent, error)
	GetCronAgent(ctx context.Context, id string) (store.CronAgent, error)
	GetTemplate(ctx context.Context, id string) (store.Template, error)
	ListScheduleRunsByCronAgent(ctx context.Context, cronAgentID string) ([]store.ScheduleRun, error)
	ListScheduleRunsByStatus(ctx context.Context, status string) ([]store.ScheduleRun, error)
	CreateScheduleRun(ctx context.Context, cronAgentID, agentID string) (store.ScheduleRun, error)
	FinishScheduleRun(ctx context.Context, id, status string) (store.ScheduleRun, error)
	CreateFromCronAgent(ctx context.Context, ca store.CronAgent) (store.Agent, error)
	CheckScheduledRunStatus(ctx context.Context, run store.ScheduleRun) (status string, err error)
	ApplyOnComplete(ctx context.Context, ca store.CronAgent, run store.ScheduleRun) error
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

// runSchedulingTickOnce is one tick's body: fire every due, enabled cron
// agent, then check every in-flight run for completion. Each half
// processes every eligible item regardless of an earlier one's failure --
// one cron agent's CreateFromCronAgent error, or one run's completion-check
// error, is logged and the loop continues, mirroring teardown's own
// "collect every error, never stop at the first" stance.
func runSchedulingTickOnce(ctx context.Context, deps schedulingDeps, log *slog.Logger) {
	fireDueCronAgents(ctx, deps, log)
	checkRunningSchedules(ctx, deps, log)
}

func fireDueCronAgents(ctx context.Context, deps schedulingDeps, log *slog.Logger) {
	cronAgents, err := deps.ListCronAgents(ctx)
	if err != nil {
		log.Warn("scheduling tick: listing cron agents", "error", err)
		return
	}
	now := time.Now()
	for _, ca := range cronAgents {
		if ca.Schedule == "" || !ca.Enabled {
			continue
		}
		runs, err := deps.ListScheduleRunsByCronAgent(ctx, ca.ID)
		if err != nil {
			log.Warn("scheduling tick: listing runs for cron agent", "cron_agent_id", ca.ID, "error", err)
			continue
		}
		var lastRun *store.ScheduleRun
		if len(runs) > 0 {
			lastRun = &runs[0] // newest first, per ListScheduleRunsByCronAgent's contract
		}
		if !agent.CronAgentDue(ca, lastRun, now) {
			continue
		}

		// Resolve the referenced Template to run the same opencode-defensive
		// check #233 had on Template.Harness directly -- CronAgent itself
		// carries no Harness field, it inherits one transitively through
		// the Template it references.
		tmpl, err := deps.GetTemplate(ctx, ca.TemplateID)
		if err != nil {
			log.Warn("scheduling tick: loading template for cron agent", "cron_agent_id", ca.ID, "template_id", ca.TemplateID, "error", err)
			continue
		}
		if tmpl.Harness == config.HarnessOpenCode {
			log.Warn("scheduling tick: cron agent's template has the opencode harness, which scheduling does not support; skipping", "cron_agent_id", ca.ID)
			continue
		}

		a, err := deps.CreateFromCronAgent(ctx, ca)
		if err != nil {
			// A failed attempt still gets a ScheduleRun (with an empty
			// AgentID), finalized as an error right away: it shows up in
			// the run history, and it advances CronAgentDue's anchor so a
			// persistently-failing cron agent is retried at its next cron
			// fire, not on every single tick.
			log.Warn("scheduling tick: creating agent from cron agent", "cron_agent_id", ca.ID, "error", err)
			run, rerr := deps.CreateScheduleRun(ctx, ca.ID, "")
			if rerr != nil {
				log.Warn("scheduling tick: recording a failed schedule run", "cron_agent_id", ca.ID, "error", rerr)
				continue
			}
			if _, ferr := deps.FinishScheduleRun(ctx, run.ID, store.ScheduleRunStatusError); ferr != nil {
				log.Warn("scheduling tick: finalizing a failed schedule run", "run_id", run.ID, "error", ferr)
			}
			continue
		}
		if _, err := deps.CreateScheduleRun(ctx, ca.ID, a.ID); err != nil {
			log.Warn("scheduling tick: recording schedule run", "cron_agent_id", ca.ID, "agent_id", a.ID, "error", err)
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
		if time.Since(run.StartedAt) > maxScheduledRunDuration {
			log.Warn("scheduling tick: run exceeded the maximum duration; finalizing as error", "run_id", run.ID, "started_at", run.StartedAt)
			finishScheduledRun(ctx, deps, log, run, store.ScheduleRunStatusError)
			continue
		}

		status, err := deps.CheckScheduledRunStatus(ctx, run)
		if err != nil {
			log.Warn("scheduling tick: checking run status", "run_id", run.ID, "error", err)
			continue
		}

		switch status {
		case "running":
			continue
		case "failed":
			finishScheduledRun(ctx, deps, log, run, store.ScheduleRunStatusError)
			continue
		case "gone":
			if _, err := deps.FinishScheduleRun(ctx, run.ID, store.ScheduleRunStatusError); err != nil {
				log.Warn("scheduling tick: finalizing a gone run as error", "run_id", run.ID, "error", err)
			}
			continue
		}
		// status == "done"
		finishScheduledRun(ctx, deps, log, run, store.ScheduleRunStatusSuccess)
	}
}

// finishScheduledRun applies run's cron agent's OnComplete policy, then
// finalizes run with status. Shared by the done, failed and timed-out
// paths, which differ only in that final status.
func finishScheduledRun(ctx context.Context, deps schedulingDeps, log *slog.Logger, run store.ScheduleRun, status string) {
	// A CronAgent deleted while this run was in flight -- allowed, since a
	// CronAgent's own lifecycle is independent of both its runs and their
	// agents -- leaves ca as the zero value here, whose OnComplete ("")
	// ApplyOnComplete's switch treats as "leave" (its safest no-op
	// default). The run still gets finalized below either way; only the
	// cleanup policy silently becomes "leave" instead of whatever the
	// deleted cron agent specified.
	ca, err := deps.GetCronAgent(ctx, run.CronAgentID)
	if err != nil && !store.IsCronAgentNotFound(err) {
		log.Warn("scheduling tick: loading cron agent for finished run", "run_id", run.ID, "cron_agent_id", run.CronAgentID, "error", err)
		return
	}
	if err := deps.ApplyOnComplete(ctx, ca, run); err != nil {
		log.Warn("scheduling tick: applying on-complete policy", "run_id", run.ID, "error", err)
	}
	if _, err := deps.FinishScheduleRun(ctx, run.ID, status); err != nil {
		log.Warn("scheduling tick: finalizing schedule run", "run_id", run.ID, "error", err)
	}
}
```

- [ ] **Step 4: Run the full cmd/docker-operator test suite**

```sh
docker run --rm -u node -v /workspace:/work -w /work/ai-sandbox/docker-operator golang:1.25 \
  go test ./cmd/docker-operator/... -v
```
Expected: PASS, every test. Also confirm `main.go` needs no change:
```sh
grep -n "startSchedulingTicker" cmd/docker-operator/main.go
```
Expected: one call site, passing a `*agent.Manager` as `deps` — compiles as long as `*agent.Manager` (from Task 2) satisfies the new `schedulingDeps` interface, which `go build` below confirms.

```sh
docker run --rm -u node -v /workspace:/work -w /work/ai-sandbox/docker-operator golang:1.25 \
  go build ./...
```
Expected: clean build across the whole module.

- [ ] **Step 5: Commit**

```bash
git add cmd/docker-operator/schedule.go cmd/docker-operator/schedule_test.go
git commit -m "feat(docker-operator): ticker fires CronAgents instead of Templates

fireDueTemplates becomes fireDueCronAgents, iterating CronAgent records
and resolving each one's Template only to read its config/Harness.
checkRunningSchedules and finishScheduledRun now load GetCronAgent
instead of GetTemplate for the completion-policy lookup, with the same
owning-record-deleted-mid-flight fallback #233 established.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 4: API layer — revert Template validation/routes, new CronAgent handlers

**Files:**
- Modify: `internal/api/templates.go`
- Create: `internal/api/cronagents.go`
- Modify: `internal/api/handlers.go`
- Modify: `internal/api/errors.go`
- Modify: `internal/api/templates_test.go`
- Create: `internal/api/cronagents_test.go`
- Modify: `internal/api/handlers_test.go`

**Interfaces:**
- Consumes: every `store.*`/`agent.*`/`Manager.*` name Tasks 1-2 produced.
- Produces: `AgentManager` interface gains `CreateCronAgent/GetCronAgent-via-ListCronAgents.../UpdateCronAgent/DeleteCronAgent/ListScheduleRunsByCronAgent` (see exact method list in Step 5) and loses `ListScheduleRunsByTemplate`; routes `GET/POST /api/cron-agents`, `PUT/DELETE /api/cron-agents/{id}`, `GET /api/cron-agents/{id}/runs`; `GET /api/templates/{id}/runs` is REMOVED. Task 5 (web) depends on this exact route shape.

- [ ] **Step 1: Write the failing tests for the Template revert**

In `internal/api/templates_test.go`, delete these five tests (they test scheduling validation that no longer exists on Template): `TestValidateTemplateFields_ScheduleRequiresHarnessTaskOnComplete`, `TestValidateTemplateFields_ScheduleValid`, `TestValidateTemplateFields_MalformedCronRejected`, `TestValidateTemplateFields_UnknownOnCompleteRejected`, `TestValidateTemplateFields_OpencodeWithScheduleRejected`. Delete `TestValidateTemplateFields_NoScheduleIgnoresTheRest` too (meaningless once Schedule doesn't exist on the request at all). Delete `TestHandleListScheduleRuns` and `TestHandleListScheduleRuns_EmptyForUnknownTemplate` (the route they test is removed in Step 3 — Task's cronagents_test.go gets the equivalent coverage under `/api/cron-agents/{id}/runs`). In `TestTemplatesMethodNotAllowed`, remove the `{"POST", "/api/templates/tpl_00000001"}`... actually check that test's table for a `/runs` path entry and remove it if present (grep `runs` in that test function first).

Add one new test confirming `templateRequest`/`validateTemplateFields` no longer accept the removed fields (a decode-time proof, not a validation proof, since `decodeJSON` rejects unknown fields):

```go
func TestCreateTemplate_RejectsUnknownScheduleField(t *testing.T) {
	h := newTestHandler(newFakeManager(5), nil)
	req := httptest.NewRequest("POST", "/api/templates", strings.NewReader(`{"name":"t","schedule":"0 9 * * *"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (schedule is no longer a template field)", rec.Code, http.StatusBadRequest)
	}
}
```

Check this file's existing imports already include `httptest`/`strings`/`http` (it almost certainly does, given the rest of the suite uses `doJSON`) — if `doJSON` already wraps this exact pattern, use `doJSON` instead and match its existing call style elsewhere in the file rather than hand-rolling `httptest.NewRequest`.

- [ ] **Step 2: Run to verify compile/behavior before the revert**

```sh
docker run --rm -u node -v /workspace:/work -w /work/ai-sandbox/docker-operator golang:1.25 \
  go test ./internal/api/... -run TestCreateTemplate_RejectsUnknownScheduleField -v
```
Expected: this specific test PASSES already (unknown-field rejection is pre-existing `decodeJSON` behavior, unaffected by this task) — the real compile break comes from the DELETED tests still referencing symbols Step 3 removes; that shows up only once Step 3 lands, confirmed in Step 6.

- [ ] **Step 3: Revert templates.go**

In `internal/api/templates.go`:
- Remove `Schedule`/`Enabled`/`Task`/`OnComplete` from `templateRequest`.
- Remove the `Harness`/`Schedule`/`Enabled`/`Task`/`OnComplete` lines from `toTemplateCreateSpec` EXCEPT keep `Harness: t.Harness,` (Harness stays on Template per decision 2):
```go
func (t templateRequest) toTemplateCreateSpec() store.TemplateCreateSpec {
	return store.TemplateCreateSpec{
		Name:                 t.Name,
		Description:          t.Description,
		Backend:              t.Backend,
		Model:                t.Model,
		FastModel:            t.FastModel,
		OllamaURL:            t.OllamaURL,
		Repo:                 t.Repo,
		AutoCompactThreshold: t.AutoCompactThreshold,
		MaxContextTokens:     t.MaxContextTokens,
		ImageTag:             t.ImageTag,
		AutoMode:             t.AutoMode,
		Harness:              t.Harness,
	}
}
```
- Remove `validOnComplete` (now dead — moves to `cronagents.go` in Step 4, with the same body).
- Simplify `validateTemplateFields` back to its pre-#233 shape, but still validating `Harness` as a plain enum (decision 2 keeps it, with nothing left to cross-check against):
```go
func validateTemplateFields(req templateRequest) *apiErr {
	if strings.TrimSpace(req.Name) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"name" must not be empty`, "name"}
	}
	if ae := validateAgentFields(req.toAgentFieldsRequest()); ae != nil {
		return ae
	}
	if req.Harness != "" && !config.ValidHarness(req.Harness) {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"harness" must be "claude-code" or "opencode"`, "harness"}
	}
	return nil
}
```
- Add the `ErrTemplateInUse` case to `templateError`:
```go
func (h *Handler) templateError(w http.ResponseWriter, action string, err error) {
	switch {
	case store.IsTemplateNotFound(err):
		writeError(w, http.StatusNotFound, CodeNotFound, "no such template", "")
	case store.IsTemplateNameExists(err):
		writeError(w, http.StatusConflict, CodeDuplicateName, "a template with that name already exists", "name")
	case store.IsTemplateInUse(err):
		writeError(w, http.StatusConflict, CodeTemplateInUse, err.Error(), "")
	default:
		h.internalError(w, action, err)
	}
}
```
- Delete `scheduleRunListResponse` and `handleListScheduleRuns` entirely (both move to `cronagents.go` under the CronAgent's own run-history route).
- `handleCreateTemplate`/`handleUpdateTemplate`/`handleDeleteTemplate`/`handleListTemplates` are otherwise unchanged.

- [ ] **Step 4: Add CodeTemplateInUse to errors.go**

In `internal/api/errors.go`'s `const` block, add, right after `CodeImageTagInUse`:
```go
	// CodeTemplateInUse is returned (409) by DELETE /api/templates/{id}
	// when some CronAgent still references the template by TemplateID.
	CodeTemplateInUse = "template_in_use"
```

- [ ] **Step 5: Write cronagents.go**

Create `internal/api/cronagents.go`, mirroring `templates.go`'s shape:

```go
package api

import (
	"net/http"
	"strings"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// cronAgentListResponse is the GET /api/cron-agents body. CronAgents is
// never nil, matching templateListResponse's wrapped-array convention.
type cronAgentListResponse struct {
	CronAgents []store.CronAgent `json:"cron_agents"`
}

// cronAgentRequest is the POST /api/cron-agents and PUT
// /api/cron-agents/{id} body.
type cronAgentRequest struct {
	Name       string `json:"name"`
	TemplateID string `json:"template_id"`
	Schedule   string `json:"schedule"`
	Enabled    bool   `json:"enabled"`
	Task       string `json:"task"`
	OnComplete string `json:"on_complete"`
}

func (r cronAgentRequest) toCronAgentCreateSpec() store.CronAgentCreateSpec {
	return store.CronAgentCreateSpec{
		Name: r.Name, TemplateID: r.TemplateID, Schedule: r.Schedule,
		Enabled: r.Enabled, Task: r.Task, OnComplete: r.OnComplete,
	}
}

// validOnComplete reports whether s is one of the three values
// ApplyOnComplete understands. Moved here from templates.go, now that
// OnComplete lives on CronAgent, not Template.
func validOnComplete(s string) bool {
	return s == "stop" || s == "delete" || s == "leave"
}

// validateCronAgentFields runs the request's own shape checks: Name and
// TemplateID required, Schedule required and parseable with the same
// parser the ticker uses, Task required, OnComplete one of the three known
// values. It does NOT check that TemplateID references an existing
// Template, or that Template's Harness -- those need a store lookup, so
// they live in the handler (handleCreateCronAgent/handleUpdateCronAgent),
// the same division of labor validateAgentFields/CreateTemplate already
// have between pure field-shape checks and store-backed existence checks.
func validateCronAgentFields(req cronAgentRequest) *apiErr {
	if strings.TrimSpace(req.Name) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"name" must not be empty`, "name"}
	}
	if strings.TrimSpace(req.TemplateID) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"template_id" must not be empty`, "template_id"}
	}
	if strings.TrimSpace(req.Schedule) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"schedule" must not be empty`, "schedule"}
	}
	if _, err := agent.ParseSchedule(req.Schedule); err != nil {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"schedule" must be a valid standard 5-field cron expression`, "schedule"}
	}
	if strings.TrimSpace(req.Task) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"task" must not be empty`, "task"}
	}
	if !validOnComplete(req.OnComplete) {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"on_complete" must be "stop", "delete" or "leave"`, "on_complete"}
	}
	return nil
}

// cronAgentError maps a store.CronAgent sentinel error to the right HTTP
// response, mirroring templateError's shape.
func (h *Handler) cronAgentError(w http.ResponseWriter, action string, err error) {
	switch {
	case store.IsCronAgentNotFound(err):
		writeError(w, http.StatusNotFound, CodeNotFound, "no such cron agent", "")
	default:
		h.internalError(w, action, err)
	}
}

// lookUpTemplateForCronAgent resolves req.TemplateID and checks its
// Harness, writing the response itself on failure. Returns ok=false when
// it already wrote a response -- the caller returns immediately in that
// case. Shared by handleCreateCronAgent and handleUpdateCronAgent.
func (h *Handler) lookUpTemplateForCronAgent(w http.ResponseWriter, r *http.Request, templateID string) (ok bool) {
	tmpl, err := h.mgr.GetTemplate(r.Context(), templateID)
	if err != nil {
		if store.IsTemplateNotFound(err) {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"template_id" does not reference an existing template`, "template_id")
			return false
		}
		h.internalError(w, "looking up template for cron agent", err)
		return false
	}
	if tmpl.Harness == "opencode" {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, `scheduling is not supported for the "opencode" harness yet`, "template_id")
		return false
	}
	return true
}

func (h *Handler) handleListCronAgents(w http.ResponseWriter, r *http.Request) {
	cronAgents, err := h.mgr.ListCronAgents(r.Context())
	if err != nil {
		h.internalError(w, "listing cron agents", err)
		return
	}
	writeJSON(w, http.StatusOK, cronAgentListResponse{CronAgents: cronAgents})
}

func (h *Handler) handleCreateCronAgent(w http.ResponseWriter, r *http.Request) {
	var req cronAgentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	if ae := validateCronAgentFields(req); ae != nil {
		ae.write(w)
		return
	}
	if !h.lookUpTemplateForCronAgent(w, r, req.TemplateID) {
		return
	}

	ca, err := h.mgr.CreateCronAgent(r.Context(), req.toCronAgentCreateSpec())
	if err != nil {
		h.cronAgentError(w, "creating cron agent", err)
		return
	}
	w.Header().Set("Location", "/api/cron-agents/"+ca.ID)
	writeJSON(w, http.StatusCreated, ca)
}

func (h *Handler) handleUpdateCronAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req cronAgentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	if ae := validateCronAgentFields(req); ae != nil {
		ae.write(w)
		return
	}
	if !h.lookUpTemplateForCronAgent(w, r, req.TemplateID) {
		return
	}

	ca, err := h.mgr.UpdateCronAgent(r.Context(), id, req.toCronAgentCreateSpec())
	if err != nil {
		h.cronAgentError(w, "updating cron agent "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, ca)
}

// handleDeleteCronAgent always answers 200, whether or not the cron agent
// existed, matching handleDeleteTemplate's idempotent stance.
func (h *Handler) handleDeleteCronAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.mgr.DeleteCronAgent(r.Context(), id); err != nil {
		h.internalError(w, "deleting cron agent "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "deleted"})
}

func (h *Handler) handleListCronAgentScheduleRuns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	runs, err := h.mgr.ListScheduleRunsByCronAgent(r.Context(), id)
	if err != nil {
		h.internalError(w, "listing schedule runs for cron agent "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, scheduleRunListResponse{Runs: runs})
}

// scheduleRunListResponse is the GET /api/cron-agents/{id}/runs body, same
// wrapped-array convention as cronAgentListResponse. Moved here from
// templates.go now that run history belongs to CronAgent, not Template.
type scheduleRunListResponse struct {
	Runs []store.ScheduleRun `json:"runs"`
}
```

- [ ] **Step 6: Wire routes and the AgentManager interface in handlers.go**

In `internal/api/handlers.go`'s `AgentManager` interface, replace:
```go
	ListScheduleRunsByTemplate(ctx context.Context, templateID string) ([]store.ScheduleRun, error)
```
with:
```go
	// CreateCronAgent/GetCronAgent-via-ListCronAgents/UpdateCronAgent/
	// DeleteCronAgent back the saved recurring-job definitions a user
	// manages on the Cron Agents page. None of these touch Docker.
	ListCronAgents(ctx context.Context) ([]store.CronAgent, error)
	CreateCronAgent(ctx context.Context, spec store.CronAgentCreateSpec) (store.CronAgent, error)
	UpdateCronAgent(ctx context.Context, id string, spec store.CronAgentCreateSpec) (store.CronAgent, error)
	DeleteCronAgent(ctx context.Context, id string) error

	// ListScheduleRunsByCronAgent backs GET /api/cron-agents/{id}/runs, a
	// cron agent's ScheduleRun history (newest first). An unknown
	// cronAgentID reads back as an empty list.
	ListScheduleRunsByCronAgent(ctx context.Context, cronAgentID string) ([]store.ScheduleRun, error)
```

Replace the route registration block:
```go
	mux.HandleFunc("GET /api/templates", h.handleListTemplates)
	mux.HandleFunc("POST /api/templates", h.handleCreateTemplate)
	mux.HandleFunc("PUT /api/templates/{id}", h.handleUpdateTemplate)
	mux.HandleFunc("DELETE /api/templates/{id}", h.handleDeleteTemplate)
	mux.HandleFunc("GET /api/templates/{id}/runs", h.handleListScheduleRuns)
```
with:
```go
	mux.HandleFunc("GET /api/templates", h.handleListTemplates)
	mux.HandleFunc("POST /api/templates", h.handleCreateTemplate)
	mux.HandleFunc("PUT /api/templates/{id}", h.handleUpdateTemplate)
	mux.HandleFunc("DELETE /api/templates/{id}", h.handleDeleteTemplate)

	mux.HandleFunc("GET /api/cron-agents", h.handleListCronAgents)
	mux.HandleFunc("POST /api/cron-agents", h.handleCreateCronAgent)
	mux.HandleFunc("PUT /api/cron-agents/{id}", h.handleUpdateCronAgent)
	mux.HandleFunc("DELETE /api/cron-agents/{id}", h.handleDeleteCronAgent)
	mux.HandleFunc("GET /api/cron-agents/{id}/runs", h.handleListCronAgentScheduleRuns)
```

And the method-not-allowed fallback block — remove `mux.HandleFunc("/api/templates/{id}/runs", methodNotAllowed)`, keep `/api/templates` and `/api/templates/{id}`, and add:
```go
	mux.HandleFunc("/api/cron-agents", methodNotAllowed)
	mux.HandleFunc("/api/cron-agents/{id}", methodNotAllowed)
	mux.HandleFunc("/api/cron-agents/{id}/runs", methodNotAllowed)
```

- [ ] **Step 7: Update fakeManager in handlers_test.go**

Remove `ListScheduleRunsByTemplate` and its doc comment from `fakeManager`. Add, mirroring the Template fakes exactly (the `templates`/`nextTemplateID`/`*TemplateErr` fields stay — `CreateTemplate`'s fake body in Step-1-of-this-task's file needs no change, since it never set the removed fields in the first place):

```go
	cronAgents       map[string]store.CronAgent
	nextCronAgentID  int
	listCronAgentsErr  error
	createCronAgentErr error
	updateCronAgentErr error
	deleteCronAgentErr error
```

Add to `newFakeManager`'s literal: `cronAgents: map[string]store.CronAgent{},`.

Add the fake methods, mirroring `ListTemplates`/`CreateTemplate`/`UpdateTemplate`/`DeleteTemplate`/`ListScheduleRunsByTemplate`:

```go
func (f *fakeManager) ListCronAgents(_ context.Context) ([]store.CronAgent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listCronAgentsErr != nil {
		return nil, f.listCronAgentsErr
	}
	out := make([]store.CronAgent, 0, len(f.cronAgents))
	for _, ca := range f.cronAgents {
		out = append(out, ca)
	}
	return out, nil
}

func (f *fakeManager) CreateCronAgent(_ context.Context, spec store.CronAgentCreateSpec) (store.CronAgent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createCronAgentErr != nil {
		return store.CronAgent{}, f.createCronAgentErr
	}
	f.nextCronAgentID++
	ca := store.CronAgent{
		ID: fmt.Sprintf("ca_%08d", f.nextCronAgentID), Name: spec.Name, TemplateID: spec.TemplateID,
		Schedule: spec.Schedule, Enabled: spec.Enabled, Task: spec.Task, OnComplete: spec.OnComplete,
	}
	f.cronAgents[ca.ID] = ca
	return ca, nil
}

func (f *fakeManager) UpdateCronAgent(_ context.Context, id string, spec store.CronAgentCreateSpec) (store.CronAgent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateCronAgentErr != nil {
		return store.CronAgent{}, f.updateCronAgentErr
	}
	ca, ok := f.cronAgents[id]
	if !ok {
		return store.CronAgent{}, fmt.Errorf("updating cron agent %q: %w", id, store.ErrCronAgentNotFound)
	}
	ca.Name, ca.TemplateID, ca.Schedule, ca.Enabled, ca.Task, ca.OnComplete =
		spec.Name, spec.TemplateID, spec.Schedule, spec.Enabled, spec.Task, spec.OnComplete
	f.cronAgents[id] = ca
	return ca, nil
}

func (f *fakeManager) DeleteCronAgent(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteCronAgentErr != nil {
		return f.deleteCronAgentErr
	}
	delete(f.cronAgents, id)
	return nil
}

// ListScheduleRunsByCronAgent mirrors ListCronAgents's style: a nil map
// entry reads as an empty, never-nil slice.
func (f *fakeManager) ListScheduleRunsByCronAgent(_ context.Context, cronAgentID string) ([]store.ScheduleRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	runs := f.scheduleRuns[cronAgentID]
	out := make([]store.ScheduleRun, len(runs))
	copy(out, runs)
	return out, nil
}
```

(`f.scheduleRuns` already exists as `map[string][]store.ScheduleRun` — it is now keyed by cron agent ID instead of template ID, a semantic-only change needing no struct edit.)

- [ ] **Step 8: Write cronagents_test.go**

Create `internal/api/cronagents_test.go`, mirroring `templates_test.go`'s shape (check that file's `doJSON` helper and `decodeEnvelope` helper signatures first — reuse them verbatim, do not redefine):

```go
package api

import (
	"net/http"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

func seedTemplateForCronAgent(t *testing.T, mgr *fakeManager) store.Template {
	t.Helper()
	tmpl, err := mgr.CreateTemplate(nil, store.TemplateCreateSpec{Name: "base config", Harness: "claude-code"})
	if err != nil {
		t.Fatalf("seeding a template: %v", err)
	}
	return tmpl
}

func TestCreateCronAgent(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)

	rec := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{
		Name: "nightly", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Enabled: true, Task: "go", OnComplete: "stop",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var ca store.CronAgent
	decodeInto(t, rec, &ca)
	if ca.Name != "nightly" || ca.TemplateID != tmpl.ID || ca.Schedule != "0 9 * * *" {
		t.Errorf("created cron agent = %+v, want the submitted fields", ca)
	}
	if got := rec.Header().Get("Location"); got != "/api/cron-agents/"+ca.ID {
		t.Errorf("Location header = %q, want %q", got, "/api/cron-agents/"+ca.ID)
	}
}

func TestCreateCronAgent_MissingName(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	rec := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "stop"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestCreateCronAgent_UnknownTemplateRejected(t *testing.T) {
	h := newTestHandler(newFakeManager(5), nil)
	rec := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{
		Name: "nightly", TemplateID: "tpl_missing", Schedule: "0 9 * * *", Task: "go", OnComplete: "stop",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (template_id does not reference an existing template)", rec.Code, http.StatusBadRequest)
	}
}

func TestCreateCronAgent_MalformedCronRejected(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	rec := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{
		Name: "nightly", TemplateID: tmpl.ID, Schedule: "not a cron expression", Task: "go", OnComplete: "stop",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestCreateCronAgent_UnknownOnCompleteRejected(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	rec := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{
		Name: "nightly", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "nonsense",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestCreateCronAgent_OpencodeTemplateRejected(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl, err := mgr.CreateTemplate(nil, store.TemplateCreateSpec{Name: "opencode base", Harness: "opencode"})
	if err != nil {
		t.Fatalf("seeding an opencode template: %v", err)
	}
	rec := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{
		Name: "nightly", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "stop",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (scheduling is claude-code only)", rec.Code, http.StatusBadRequest)
	}
}

func TestListCronAgents(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{Name: "one", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "stop"})
	doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{Name: "two", TemplateID: tmpl.ID, Schedule: "0 10 * * *", Task: "go", OnComplete: "stop"})

	rec := doJSON(t, h, "GET", "/api/cron-agents", nil)
	var body cronAgentListResponse
	decodeInto(t, rec, &body)
	if len(body.CronAgents) != 2 {
		t.Errorf("CronAgents = %+v, want 2", body.CronAgents)
	}
}

func TestUpdateCronAgent(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	created := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{Name: "original", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "stop"})
	var ca store.CronAgent
	decodeInto(t, created, &ca)

	rec := doJSON(t, h, "PUT", "/api/cron-agents/"+ca.ID, cronAgentRequest{
		Name: "renamed", TemplateID: tmpl.ID, Schedule: "0 10 * * *", Enabled: true, Task: "go now", OnComplete: "delete",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var updated store.CronAgent
	decodeInto(t, rec, &updated)
	if updated.Name != "renamed" || updated.Schedule != "0 10 * * *" || updated.OnComplete != "delete" {
		t.Errorf("updated cron agent = %+v, want the new fields", updated)
	}
}

func TestUpdateCronAgent_NotFound(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	rec := doJSON(t, h, "PUT", "/api/cron-agents/ca_missing", cronAgentRequest{
		Name: "x", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "stop",
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestDeleteCronAgent(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	created := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{Name: "t", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "stop"})
	var ca store.CronAgent
	decodeInto(t, created, &ca)

	rec := doJSON(t, h, "DELETE", "/api/cron-agents/"+ca.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	list := doJSON(t, h, "GET", "/api/cron-agents", nil)
	var body cronAgentListResponse
	decodeInto(t, list, &body)
	if len(body.CronAgents) != 0 {
		t.Errorf("CronAgents after delete = %+v, want empty", body.CronAgents)
	}
}

func TestDeleteCronAgent_IsIdempotent(t *testing.T) {
	h := newTestHandler(newFakeManager(5), nil)
	rec := doJSON(t, h, "DELETE", "/api/cron-agents/ca_never_existed", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (idempotent)", rec.Code, http.StatusOK)
	}
}

func TestHandleListCronAgentScheduleRuns(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.scheduleRuns = map[string][]store.ScheduleRun{
		"ca_1": {{ID: "run_1", CronAgentID: "ca_1", AgentID: "agt_1", Status: store.ScheduleRunStatusSuccess}},
	}
	h := newTestHandler(mgr, nil)
	rec := doJSON(t, h, "GET", "/api/cron-agents/ca_1/runs", nil)
	var body scheduleRunListResponse
	decodeInto(t, rec, &body)
	if len(body.Runs) != 1 || body.Runs[0].ID != "run_1" {
		t.Errorf("Runs = %+v, want [run_1]", body.Runs)
	}
}

func TestHandleListCronAgentScheduleRuns_EmptyForUnknownCronAgent(t *testing.T) {
	h := newTestHandler(newFakeManager(5), nil)
	rec := doJSON(t, h, "GET", "/api/cron-agents/ca_missing/runs", nil)
	var body scheduleRunListResponse
	decodeInto(t, rec, &body)
	if len(body.Runs) != 0 {
		t.Errorf("Runs = %+v, want empty for an unknown cron agent", body.Runs)
	}
}

func TestCronAgentsMethodNotAllowed(t *testing.T) {
	h := newTestHandler(newFakeManager(5), nil)
	cases := []struct{ method, path string }{
		{"DELETE", "/api/cron-agents"},
		{"PUT", "/api/cron-agents"},
		{"POST", "/api/cron-agents/ca_00000001"},
	}
	for _, c := range cases {
		rec := doJSON(t, h, c.method, c.path, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status = %d, want %d", c.method, c.path, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}
```

Before writing this, grep `internal/api/templates_test.go` for `func doJSON` and `func decodeEnvelope`/a response-decoding helper's exact name and signature (the sketch above assumes `decodeInto(t, rec, &v)` exists or is trivially close to an existing helper — match whatever this file's existing helpers are actually named, do not invent a second one if one already serves this purpose).

- [ ] **Step 9: Run the full api package test suite**

```sh
docker run --rm -u node -v /workspace:/work -w /work/ai-sandbox/docker-operator golang:1.25 \
  go test ./internal/api/... -v
```
Expected: PASS, every test.

- [ ] **Step 10: Run the whole-module build and vet**

```sh
docker run --rm -u node -v /workspace:/work -w /work/ai-sandbox/docker-operator golang:1.25 \
  sh -c "go build ./... && go vet ./..."
```
Expected: clean.

- [ ] **Step 11: Commit**

```bash
git add internal/api/templates.go internal/api/cronagents.go internal/api/handlers.go \
  internal/api/errors.go internal/api/templates_test.go internal/api/cronagents_test.go \
  internal/api/handlers_test.go
git commit -m "feat(api): /api/cron-agents routes, Template validation reverted

Template's create/update validation drops its scheduling checks; Harness
stays. New /api/cron-agents CRUD plus /api/cron-agents/{id}/runs
(replacing /api/templates/{id}/runs). DeleteTemplate's new 409 surfaces
as CodeTemplateInUse. validateCronAgentFields checks template_id against
a real store lookup, not just non-empty, and follows the referenced
Template's Harness for the opencode rejection.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 5: Web UI — revert Templates page, new Cron Agents page

**Files:**
- Modify: `web/render.js`
- Modify: `web/render.test.js`
- Modify: `web/templates.js`
- Create: `web/cronagents.js`
- Modify: `web/index.html`
- Modify: `web/app.js`
- Modify: `web/style.css`

**Interfaces:**
- Consumes: `GET/POST /api/templates`, `PUT/DELETE /api/templates/{id}` (unchanged shape, minus scheduling fields), `GET/POST /api/cron-agents`, `PUT/DELETE /api/cron-agents/{id}`, `GET /api/cron-agents/{id}/runs` (Task 4).
- Produces: `window.Render.renderTemplateForm` (reverted), `window.Render.renderCronAgentsPage`/`renderCronAgentForm` (new, exported from `render.js` the same way `renderTemplatesPage`/`renderTemplateForm` are), `window.renderCronAgentsPage(container)` (new, from `cronagents.js`, mirroring `window.renderTemplatesPage`).

- [ ] **Step 1: Write the failing render.test.js tests**

In `web/render.test.js`:

Remove the three scheduling-specific template-form tests whose premise disappears (`renderTemplateForm includes the five new scheduling fields`, `renderTemplateForm: two labeled sections group config vs. scheduling fields` — the second one's "two sections" claim no longer holds once Scheduling is removed, collapsing to one). Keep `renderTemplateForm: Backend and Auto mode are selects, not free-text inputs` and `renderTemplateForm: rarely-used fields are grouped under a collapsed Advanced disclosure` — both describe behavior that survives the revert unchanged. Add:

```js
test('renderTemplateForm: no longer renders a Scheduling section', () => {
	const html = Render.renderTemplateForm({});
	assert.ok(!html.includes('Scheduling'));
	assert.ok(!html.includes('name="schedule"'));
	assert.ok(!html.includes('name="task"'));
	assert.ok(!html.includes('name="on_complete"'));
	assert.ok(!html.includes('name="enabled"'));
});

test('renderCronAgentsPage lists cron agents with a schedule summary', () => {
	const html = Render.renderCronAgentsPage([
		{ id: 'ca_1', name: 'nightly', schedule: '0 9 * * *', enabled: true },
		{ id: 'ca_2', name: 'weekly', schedule: '', enabled: false },
	]);
	assert.ok(html.includes('nightly'));
	assert.ok(html.includes('0 9 * * *'));
	assert.ok(html.includes('(enabled)'));
});

test('renderCronAgentsPage: empty/null list still renders the shell', () => {
	assert.ok(Render.renderCronAgentsPage([]).includes('cron-agents-page'));
	assert.ok(Render.renderCronAgentsPage(null).includes('cron-agents-page'));
});

test('renderCronAgentForm: renders Name, a Template select, Schedule, Enabled, Task, On complete', () => {
	const html = Render.renderCronAgentForm(
		{ id: 'ca_1', name: 'nightly', template_id: 'tpl_1', schedule: '0 9 * * *', enabled: true, task: 'do it', on_complete: 'stop' },
		[{ id: 'tpl_1', name: 'base config' }, { id: 'tpl_2', name: 'other' }]
	);
	assert.ok(html.includes('name="name"'));
	assert.ok(html.includes('name="template_id"'));
	assert.ok(html.includes('<option value="tpl_1"') && html.includes('selected'));
	assert.ok(html.includes('base config'));
	assert.ok(html.includes('name="schedule"'));
	assert.ok(html.includes('name="enabled"'));
	assert.ok(html.includes('name="task"'));
	assert.ok(html.includes('name="on_complete"'));
});

test('renderCronAgentForm: a null cron agent renders a blank form with no template pre-selected', () => {
	const html = Render.renderCronAgentForm(null, [{ id: 'tpl_1', name: 'base config' }]);
	assert.ok(html.includes('<option value="">'));
	assert.ok(!html.includes('selected>base config'));
});

test('renderCronAgentForm: escapes every value', () => {
	const html = Render.renderCronAgentForm(
		{ name: '<img src=x>', template_id: 'tpl_1', schedule: '0 9 * * *', task: '<script>', on_complete: 'stop' },
		[{ id: 'tpl_1', name: '<b>bold</b>' }]
	);
	assert.ok(!html.includes('<img src=x>'));
	assert.ok(!html.includes('<script>'));
	assert.ok(!html.includes('<b>bold</b>'));
});
```

Check this file's existing import style (whether it uses `const { test } = require('node:test')` plus `const assert = require('node:assert')`, or something else) and match it exactly — do not introduce a different test-running convention.

- [ ] **Step 2: Run to verify these fail**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator node:22 \
  node --test web/render.test.js
```
Expected: FAIL — `Render.renderCronAgentsPage`/`renderCronAgentForm` don't exist yet; the "no Scheduling section" test fails against the current `renderTemplateForm`.

- [ ] **Step 3: Revert renderTemplateForm in render.js**

In `web/render.js`, remove the entire `<section class="template-form__section template-form__section--schedule">...</section>` block from `renderTemplateForm` (the Scheduling section: Enabled checkbox, Schedule input, Task textarea, On complete select) — everything between the closing `</details>` of the Advanced block and the `<div class="template-form__actions">` line. The function keeps its single `<section class="template-form__section">` ("Agent configuration") and the Advanced `<details>`, unchanged. Update the function's own doc comment to drop the "(plus renderScheduleRunList once a template has an id)" and "14 fields" language, since the Templates page no longer shows run history or scheduling fields at all.

Also revert `renderTemplatesPage`'s row markup: remove the `scheduleSummary(t)` call and its `<span class="templates-page__schedule">` output (the Templates list no longer has a schedule to summarize) — but KEEP the `scheduleSummary` function itself only if Step 4 needs it for the Cron Agents page (it does — see Step 4, which reuses the identical logic under a new name). Delete `scheduleSummary` from `renderTemplatesPage`'s usage but keep its body, renamed `cronAgentScheduleSummary`, moved next to the new `renderCronAgentsPage` (Step 4) rather than left orphaned next to `renderTemplatesPage`.

`renderScheduleRunList` is UNCHANGED (reused verbatim by the new Cron Agents page) — leave it exactly where it is in the file.

- [ ] **Step 4: Write renderCronAgentsPage and renderCronAgentForm**

Add to `web/render.js`, immediately after `renderTemplatesPage` (replacing the old `scheduleSummary` function with this renamed/relocated version):

```js
// cronAgentScheduleSummary renders a human-readable one-liner for a cron
// agent's cron schedule, or '' when it has none. Renamed/relocated from
// the old Templates-page scheduleSummary, now that the schedule lives on
// CronAgent, not Template.
function cronAgentScheduleSummary(ca) {
	if (!ca || !ca.schedule) return '';
	return escapeHTML(ca.schedule) + (ca.enabled ? ' (enabled)' : ' (paused)');
}

// renderCronAgentsPage renders the standalone Cron Agents page: every
// saved cron agent, alphabetically by name, with its schedule summary and
// Edit/Delete actions, a "New cron agent" button, a detail slot the
// Edit/New flow fills with renderCronAgentForm (plus renderScheduleRunList
// once a cron agent has an id), and an error line for a failed
// load/save/delete. Mirrors renderTemplatesPage's own shape exactly.
function renderCronAgentsPage(cronAgents) {
	var rows = (cronAgents || []).slice().sort(function (a, b) {
		var an = (a && a.name) || '';
		var bn = (b && b.name) || '';
		return an < bn ? -1 : an > bn ? 1 : 0;
	}).map(function (ca) {
		var summary = cronAgentScheduleSummary(ca);
		return (
			'<li class="cron-agents-page__row" data-cron-agent-id="' + escapeHTML(ca.id) + '">' +
				'<span class="cron-agents-page__name">' + escapeHTML(ca.name || '(unnamed cron agent)') + '</span>' +
				(summary ? '<span class="cron-agents-page__schedule">' + summary + '</span>' : '') +
				'<button class="cron-agents-page__edit btn btn--ghost btn--sm" type="button">Edit</button>' +
				'<button class="cron-agents-page__delete btn btn--danger btn--sm" type="button">Delete</button>' +
			'</li>'
		);
	}).join('');
	return (
		'<div class="cron-agents-page">' +
			'<div class="cron-agents-page__header">' +
				'<h2>Cron Agents</h2>' +
				'<button class="cron-agents-page__new btn btn--primary" type="button">New cron agent</button>' +
			'</div>' +
			'<ul class="cron-agents-page__list">' + rows + '</ul>' +
			'<div class="cron-agents-page__detail" hidden></div>' +
			'<p class="cron-agents-page__error" role="alert" hidden></p>' +
		'</div>'
	);
}

// renderCronAgentForm renders the edit form for one cron agent (or a blank
// one for "New cron agent" when ca is null/undefined): Name, a Template
// picker (populated from templates, the same list GET /api/templates
// returns), Schedule, Enabled, Task, On complete.
function renderCronAgentForm(ca, templates) {
	ca = ca || {};
	var templateOptions = '<option value="">— Select a template —</option>' +
		(templates || []).slice().sort(function (a, b) {
			var an = (a && a.name) || '';
			var bn = (b && b.name) || '';
			return an < bn ? -1 : an > bn ? 1 : 0;
		}).map(function (t) {
			var label = t.name ? escapeHTML(t.name) : '(unnamed template)';
			return '<option value="' + escapeHTML(t.id) + '"' + (ca.template_id === t.id ? ' selected' : '') + '>' + label + '</option>';
		}).join('');
	return (
		'<form class="cron-agent-form">' +
			'<label class="cron-agent-form__row">Name' +
				'<input type="text" name="name" value="' + escapeHTML(ca.name || '') + '" required>' +
			'</label>' +
			'<label class="cron-agent-form__row">Template' +
				'<select name="template_id" required>' + templateOptions + '</select>' +
			'</label>' +
			'<label class="cron-agent-form__row">Schedule (cron)' +
				'<input type="text" name="schedule" value="' + escapeHTML(ca.schedule || '') + '" placeholder="0 9 * * *">' +
				'<span class="cron-agent-form__help">Standard 5-field cron, evaluated in UTC.</span>' +
			'</label>' +
			'<label class="cron-agent-form__row cron-agent-form__row--inline">' +
				'<input type="checkbox" name="enabled"' + (ca.enabled ? ' checked' : '') + '> Enabled' +
			'</label>' +
			'<label class="cron-agent-form__row">Task' +
				'<textarea name="task">' + escapeHTML(ca.task || '') + '</textarea>' +
				'<span class="cron-agent-form__help">The prompt each triggered run gets, as a one-shot task (Claude Code only).</span>' +
			'</label>' +
			'<label class="cron-agent-form__row">On complete' +
				'<select name="on_complete">' +
					'<option value="">—</option>' +
					'<option value="stop"' + (ca.on_complete === 'stop' ? ' selected' : '') + '>Stop</option>' +
					'<option value="delete"' + (ca.on_complete === 'delete' ? ' selected' : '') + '>Delete</option>' +
					'<option value="leave"' + (ca.on_complete === 'leave' ? ' selected' : '') + '>Leave</option>' +
				'</select>' +
			'</label>' +
			'<div class="cron-agent-form__actions">' +
				'<button class="cron-agent-form__save btn btn--primary" type="submit">Save</button>' +
			'</div>' +
		'</form>'
	);
}
```

Add `renderCronAgentsPage` and `renderCronAgentForm` to the module's exports object (the same object that currently exports `renderTemplatesPage`/`renderTemplateForm`/`renderScheduleRunList` around line 1403-1405 — add two new keys there, same pattern: `renderCronAgentsPage: renderCronAgentsPage, renderCronAgentForm: renderCronAgentForm,`).

- [ ] **Step 5: Run render.test.js**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator node:22 \
  node --test web/render.test.js web/terminal.test.js web/auth.test.js web/files.test.js
```
Expected: PASS, every test (running the other three suites too catches any accidental cross-file regression from editing `render.js`, the shared pure-rendering module).

- [ ] **Step 6: Simplify templates.js (remove run-history wiring)**

In `web/templates.js`: remove `loadRuns`, `bindRuns`, and `openFiles` entirely (the Templates page no longer shows run history). In `openForm`, remove the `if (template && template.id) loadRuns(template.id, template.on_complete);` line — it becomes just:
```js
function openForm(template) {
	showDetail(window.Render.renderTemplateForm(template));
	bindDetail(template);
}
```
In `formToRequest`, remove the `enabled`/`schedule`/`task`/`on_complete` keys from the returned object (they are no longer `templateRequest` fields — Task 4's `decodeJSON` strictness means sending them would 400, since `templateRequest` no longer declares them as JSON tags, making `DisallowUnknownFields` reject the request).

- [ ] **Step 7: Write cronagents.js**

Create `web/cronagents.js`, mirroring `templates.js`'s shape exactly (DOM wiring; pure rendering stays in `render.js`):

```js
// cronagents.js -- DOM wiring for the Cron Agents page. Mirrors
// templates.js's dual-export/guard shape: pure rendering lives in
// render.js (renderCronAgentsPage / renderCronAgentForm /
// renderScheduleRunList, unit-tested), this file is the event wiring
// reviewed by hand.
//
// Exposes window.renderCronAgentsPage(container), which app.js calls when
// the sidebar "Cron Agents" button is clicked.
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
			template_id: fd.get('template_id') || '',
			schedule: fd.get('schedule') || '',
			enabled: fd.get('enabled') === 'on',
			task: fd.get('task') || '',
			on_complete: fd.get('on_complete') || '',
		};
	}

	function render(container) {
		var state = { cronAgents: [], templates: [] };

		function showError(msg) {
			var el = container.querySelector('.cron-agents-page__error');
			if (!el) return;
			el.textContent = msg;
			el.hidden = !msg;
		}

		function showDetail(html) {
			var el = container.querySelector('.cron-agents-page__detail');
			if (!el) return;
			el.innerHTML = html;
			el.hidden = !html;
		}

		function load() {
			fetchJSON('/api/cron-agents')
				.then(function (data) {
					state.cronAgents = data.cron_agents || [];
					container.innerHTML = window.Render.renderCronAgentsPage(state.cronAgents);
					bind();
				})
				.catch(function (e) {
					container.innerHTML = window.Render.renderCronAgentsPage([]);
					bind();
					showError('Could not load cron agents: ' + e.message);
				});
		}

		function loadTemplatesThenOpenForm(cronAgent) {
			fetchJSON('/api/templates')
				.then(function (data) {
					state.templates = data.templates || [];
					openForm(cronAgent);
				})
				.catch(function (e) {
					showError('Could not load templates: ' + e.message);
				});
		}

		function openForm(cronAgent) {
			showDetail(window.Render.renderCronAgentForm(cronAgent, state.templates));
			if (cronAgent && cronAgent.id) loadRuns(cronAgent.id, cronAgent.on_complete);
			bindDetail(cronAgent);
		}

		function loadRuns(cronAgentID, onComplete) {
			fetchJSON('/api/cron-agents/' + encodeURIComponent(cronAgentID) + '/runs')
				.then(function (data) {
					var detail = container.querySelector('.cron-agents-page__detail');
					if (!detail) return;
					detail.insertAdjacentHTML('beforeend', window.Render.renderScheduleRunList(data.runs || [], onComplete));
					bindRuns();
				})
				.catch(function () { /* run history is best-effort for this view */ });
		}

		// openFiles swaps this page for the Files browser, where an archived
		// transcript lives (transcripts/<id>/).
		function openFiles() {
			if (typeof window.renderFileBrowser === 'function') window.renderFileBrowser(container);
		}

		function bindRuns() {
			container.querySelectorAll('.schedule-run-list__open-agent').forEach(function (btn) {
				btn.addEventListener('click', function () {
					var id = btn.getAttribute('data-agent-id');
					if (typeof window.openAgent === 'function' && window.openAgent(id)) return;
					openFiles();
				});
			});
			container.querySelectorAll('.schedule-run-list__open-files').forEach(function (btn) {
				btn.addEventListener('click', openFiles);
			});
		}

		function bindDetail(cronAgent) {
			var form = container.querySelector('.cron-agent-form');
			if (!form) return;
			form.addEventListener('submit', function (ev) {
				ev.preventDefault();
				var body = JSON.stringify(formToRequest(form));
				var req = cronAgent && cronAgent.id
					? fetchJSON('/api/cron-agents/' + encodeURIComponent(cronAgent.id), { method: 'PUT', body: body, headers: { 'Content-Type': 'application/json' } })
					: fetchJSON('/api/cron-agents', { method: 'POST', body: body, headers: { 'Content-Type': 'application/json' } });
				req.then(function () { showDetail(''); load(); })
					.catch(function (e) { showError('Could not save cron agent: ' + e.message); });
			});
		}

		function bind() {
			var newBtn = container.querySelector('.cron-agents-page__new');
			if (newBtn) newBtn.addEventListener('click', function () { loadTemplatesThenOpenForm(null); });

			container.querySelectorAll('.cron-agents-page__edit').forEach(function (btn) {
				btn.addEventListener('click', function () {
					var id = btn.closest('[data-cron-agent-id]').getAttribute('data-cron-agent-id');
					var ca = state.cronAgents.find(function (x) { return x.id === id; });
					if (ca) loadTemplatesThenOpenForm(ca);
				});
			});

			container.querySelectorAll('.cron-agents-page__delete').forEach(function (btn) {
				btn.addEventListener('click', function () {
					var id = btn.closest('[data-cron-agent-id]').getAttribute('data-cron-agent-id');
					window.OperatorConfirm.show(
						'Deleting this cron agent permanently removes its schedule, task text and completion policy. This cannot be undone.',
						{ title: 'Delete this cron agent?', confirmLabel: 'Delete', danger: true }
					).then(function (confirmed) {
						if (!confirmed) return;
						fetchJSON('/api/cron-agents/' + encodeURIComponent(id), { method: 'DELETE' })
							.then(load)
							.catch(function (e) { showError('Could not delete cron agent: ' + e.message); });
					});
				});
			});
		}

		load();
	}

	window.renderCronAgentsPage = render;
})();
```

- [ ] **Step 8: Wire index.html and app.js**

In `web/index.html`, add a new sidebar button right after `templates-btn` and a new script tag right after `templates.js`:
```html
	<button id="templates-btn" class="templates-btn btn btn--ghost" type="button">Templates</button>
	<button id="cron-agents-btn" class="cron-agents-btn btn btn--ghost" type="button">Cron Agents</button>
```
```html
<script src="templates.js"></script>
<script src="cronagents.js"></script>
```

In `web/app.js`, add the element lookup next to `templatesBtn`'s:
```js
	var cronAgentsBtn = document.getElementById('cron-agents-btn');
```
and the click wiring right after the `templatesBtn` block, same pattern:
```js
	if (cronAgentsBtn) {
		cronAgentsBtn.addEventListener('click', function () {
			if (typeof window.teardownActiveView === 'function') window.teardownActiveView();
			state.selectedID = null;
			renderSidebar();
			if (typeof window.renderCronAgentsPage === 'function') {
				window.renderCronAgentsPage(mainArea);
			}
		});
	}
```

- [ ] **Step 9: Extend style.css**

In `web/style.css`, extend every combined selector that currently lists `.template-form__row`/`.template-form__section-heading`/etc. to also list the `.cron-agent-form__row` equivalent, following this file's own established pattern (`.create-form__row, .template-form__row` already shares one rule body — add `.cron-agent-form__row` as a third member of each of these lists):

```css
.template-form__row input[type="text"],
.template-form__row select,
.template-form__row textarea,
.cron-agent-form__row input[type="text"],
.cron-agent-form__row select,
.cron-agent-form__row textarea,
```
(and the matching `:focus` block right below it — add the same three `.cron-agent-form__row` lines).

```css
.create-form__row,
.template-form__row,
.cron-agent-form__row {
```

```css
.create-form__help,
.template-form__help,
.cron-agent-form__help { font-size: var(--font-xs); color: var(--fg-dim); }
```

For the inline-row modifier, extend:
```css
.template-form__row--inline,
.cron-agent-form__row--inline {
```

For the sticky action bar, extend:
```css
.create-form__actions,
.template-form__actions,
.cron-agent-form__actions {
```

And for the error line (not currently used by `cron-agent-form` but kept consistent for a future error row):
```css
.create-form__error,
.template-form__error,
.cron-agent-form__error { color: var(--status-error); font-size: var(--font-sm2); margin: 0; }
```

Add a new block, modeled exactly on the existing "templates page (issue #228)" block, right after it:

```css
/* --- cron agents page --------------------------------------------------
   Mirrors the templates page's own shell/list/detail chrome -- a
   CronAgent's own form rows share styling with the create/template forms
   via the combined selectors above (.cron-agent-form__row etc.). */
.cron-agents-page {
	max-width: 720px;
	margin: 24px auto;
	display: flex;
	flex-direction: column;
	gap: var(--space-5);
}

.cron-agents-page__header {
	display: flex;
	align-items: center;
	justify-content: space-between;
	gap: var(--space-4);
}

.cron-agents-page__header h2 { margin: 0; font-size: var(--font-lg); font-weight: 600; }

.cron-agents-page__list {
	list-style: none;
	margin: 0;
	padding: 0;
	border: 1px solid var(--border);
	border-radius: var(--radius-md);
	overflow: hidden;
}

.cron-agents-page__row {
	display: flex;
	align-items: center;
	gap: var(--space-4);
	padding: var(--space-4) var(--space-5);
	border-bottom: 1px solid var(--border);
}
.cron-agents-page__row:last-child { border-bottom: none; }
.cron-agents-page__row:hover { background: var(--bg-item); }

.cron-agents-page__name { font-weight: 600; flex: 1 1 auto; }

.cron-agents-page__schedule {
	font-size: var(--font-xs);
	color: var(--fg-dim);
	font-family: var(--font-mono, monospace);
}

.cron-agents-page__detail {
	border: 1px solid var(--border);
	border-radius: var(--radius-md);
	padding: var(--space-5);
}

.cron-agents-page__error { color: var(--status-error); font-size: var(--font-sm2); margin: 0; }

.cron-agent-form {
	display: flex;
	flex-direction: column;
	gap: var(--space-5);
}
```

Add the sidebar-button spacing, mirroring `.templates-btn`:
```css
.cron-agents-btn {
	margin-bottom: var(--space-5);
}
```
(place it immediately after the existing `.templates-btn { margin-bottom: var(--space-5); }` rule, around line 1065).

- [ ] **Step 10: Run the full web test suite**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator node:22 \
  node --test web/render.test.js web/terminal.test.js web/auth.test.js web/files.test.js
```
Expected: PASS, every test. `cronagents.js`/`templates.js`/`app.js`/`index.html` are hand-reviewed DOM wiring with no dedicated test file, matching this codebase's own established division (only `render.js`'s pure functions are unit-tested) — verify by manual trace of the click→fetch→render chain, not by writing a test harness this codebase has no precedent for.

- [ ] **Step 11: Commit**

```bash
git add web/render.js web/render.test.js web/templates.js web/cronagents.js \
  web/index.html web/app.js web/style.css
git commit -m "feat(web): Cron Agents page, Templates page reverted

renderTemplateForm drops its Scheduling section; the Templates page no
longer shows run history. A new Cron Agents page (render.js:
renderCronAgentsPage/renderCronAgentForm, cronagents.js: DOM wiring)
lists every CronAgent with a Template picker, schedule, task and
completion policy, reusing renderScheduleRunList unchanged for its own
run history. New sidebar button and routes.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 6: sync-web-embed, whole-module gate, final verification

**Files:**
- Modify: `internal/webui/web/*` (committed copy, regenerated — never hand-edited)

**Interfaces:**
- Consumes: every file Task 5 touched in `web/`.
- Produces: a green `make all` (or its manual equivalent below), confirming the embedded copy matches the source and nothing in the whole module regressed.

- [ ] **Step 1: Regenerate the embedded web copy by hand**

`make` is unavailable on this container in this environment — replicate `sync-web-embed`'s exact shell logic (read `docker-operator/Makefile`'s `sync-web-embed` target first with `grep -A 5 "^sync-web-embed:" Makefile` to confirm the exact commands have not changed since this plan was written, then run):

```sh
cd /workspace/ai-sandbox/docker-operator
rm -rf internal/webui/web
mkdir -p internal/webui/web
cp -a web/. internal/webui/web/
find internal/webui/web -name '*.test.js' -delete
```

- [ ] **Step 2: Verify the copy matches the source exactly (minus test files)**

```sh
diff -rq web internal/webui/web | grep -v '\.test\.js'
find internal/webui/web -name '*.test.js' | wc -l
```
Expected: `diff -rq` prints nothing (every non-test file identical); the `find` count is `0`.

- [ ] **Step 3: Run the complete Go gate**

```sh
docker run --rm -u node -v /workspace:/work -w /work/ai-sandbox/docker-operator golang:1.25 sh -c "
  go vet ./... &&
  gofmt -l . &&
  go test ./... -race
"
```
Expected: `go vet` clean, `gofmt -l .` prints nothing (no unformatted files), `go test -race` passes across every package touched by Tasks 1-4.

- [ ] **Step 4: Run golangci-lint**

Per this project's `golangci-lint-via-prebuilt-image` memory note (`go install` of the linter is DependaProxy-blocked):
```sh
docker run --rm -v /workspace/ai-sandbox/docker-operator:/work -w /work golangci/golangci-lint:latest \
  golangci-lint run ./...
```
Expected: clean.

- [ ] **Step 5: Run the full web test suite one more time, against the embedded copy's siblings**

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /workspace:/work -w /work/ai-sandbox/docker-operator node:22 \
  node --test web/render.test.js web/terminal.test.js web/auth.test.js web/files.test.js
```
Expected: PASS.

- [ ] **Step 6: Commit the embedded copy**

```bash
git add internal/webui/web
git commit -m "chore: sync embedded web copy after the CronAgent UI changes

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

- [ ] **Step 7: Manual UI smoke check (requires a browser — flag if unavailable in this environment)**

Start the operator against a throwaway state file inside a Docker container (per this project's `use-docker` skill) and, in a browser: open the new "Cron Agents" sidebar button; confirm the sidebar spacing and page layout read correctly (no overlap, matching the Templates page's own established visual language); create a Template, then create a CronAgent referencing it; confirm the created agent gets the CronAgent's `Name`; attempt to delete the referenced Template and confirm the 409/"in use" message surfaces in the Templates page's error line; delete the CronAgent, then confirm the Template can now be deleted. If no browser is available in this environment, say so explicitly rather than claiming this step passed — this mirrors the project's own "UI changes need a human's eyes" standard from its CLAUDE.md-adjacent practice in this session's prior work.

---

## Self-Review

**1. Spec coverage:**
- Decision 1 (Task/OnComplete/Enabled move to CronAgent) → Task 1 Step 3/6, Task 2 Step 5.
- Decision 2 (Harness stays on Template) → Task 1 Step 3 (kept), Task 3 Step 3 (`fireDueCronAgents` loads the Template to check it), Task 4 Step 3/5.
- Decision 3 (no data migration) → Task 1 drops/renames fields outright; Task 1 Step 1's test proves old JSON with the dropped keys still decodes.
- Decision 4 (`CronAgent.Name` becomes `Agent.Name`) → Task 2 Step 5 (`CreateFromCronAgent`'s `Name: ca.Name`).
- Decision 5 (`DeleteTemplate` blocked while referenced) → Task 1 Step 4/5.
- Decision 6 (naming) → used consistently across every task.
- Data model (Template reverted, CronAgent new, ScheduleRun renamed, Manager pass-through ordering) → Task 1 (store), Task 2 (manager, explicitly in one task per the ordering note).
- Validation/referential integrity → Task 4 Step 3/5.
- The ticker → Task 3.
- UI → Task 5.
- Testing section's every bullet → covered by the corresponding task's own Steps (store CRUD/decode-inert/409 in Task 1; CreateFromCronAgent/ApplyOnComplete/CronAgentDue in Task 2; ticker's fire/check/defensive/create-failure/gone/multiple-due guarantees in Task 3; validate/404/409 in Task 4; render tests in Task 5).

**2. Placeholder scan:** none found — every step above either contains the literal code to write or an exact shell command; no "TBD"/"similar to"/"add appropriate" phrasing.

**3. Type consistency:** traced every renamed identifier across tasks: `store.CronAgent`/`CronAgentCreateSpec`/`NewCronAgentID`/`IsCronAgentNotFound`/`IsCronAgentExists` (Task 1) match their exact use in Task 2's `cronagents.go` and Task 3's `fakeSchedulingDeps`; `agent.CronAgentDue` (Task 2, renamed from the spec's literal "TemplateDue" — called out explicitly as a deliberate naming correction) matches its use in Task 3's `fireDueCronAgents`; `(*Manager).CreateFromCronAgent`/`ApplyOnComplete`/`ListScheduleRunsByCronAgent` (Task 2) match the `schedulingDeps` interface (Task 3) and the `AgentManager` interface (Task 4) method-for-method; `store.ScheduleRun.CronAgentID` (Task 1) is used consistently (never the old `TemplateID` name) from Task 1's own tests through Task 4's `cronagents_test.go` and Task 5's `cronagents.js`/`render.test.js`; `cronAgentRequest`'s JSON field names (`template_id`, `on_complete`, ...) match `formToRequest` in `cronagents.js` and the `name=` attributes `renderCronAgentForm` emits in `render.js`.

**4. Review Focus:** all five items each have an owning task and test, listed above in the Review Focus section itself (DeleteTemplate's atomic check — Task 1; deleted-CronAgent-mid-flight finalization — Task 3; template_id existence check — Task 4; opencode-via-referenced-Template — Task 3 and Task 4 both; legacy-JSON-decodes-cleanly — Task 1).

---

Plan complete and saved to `docs/superpowers/plans/2026-10-11-cron-agents.md`. Please review the plan. Which execution approach would you prefer?

- **Subagent-driven** - A fresh subagent implements each task and a fresh reviewer checks it before the next one starts, then a whole-branch review at the end. Most thorough; costs a fresh context per task and per review.
- **Native** - I implement every task myself in this session, the way this harness runs work, then one fresh reviewer on the most capable model checks the whole branch. Cheapest and fastest; no independent review until the end. Runs well with a mid-tier session model, since the plan carries the design.

For this plan I recommend **Subagent-driven**, because the six tasks form a strict dependency chain across four different Go packages plus the web layer, each renaming the same handful of identifiers (`CronAgent`, `CronAgentID`, `CreateFromCronAgent`, `ApplyOnComplete`, `CronAgentDue`) consistently through store → manager → ticker → API → UI — exactly the kind of multi-file, interface-matching work where a fresh per-task reviewer catching a drifted signature before the next task builds on it is worth the extra cost, especially after #233's own plan already shipped once with a pass-through-ordering mistake that only surfaced mid-task via `go vet`. Does the plan capture what you want, and which approach should we use?
