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
	templates      []store.Template
	runsByTemplate map[string][]store.ScheduleRun
	runningRuns    []store.ScheduleRun
	created        []string          // template IDs CreateFromTemplate was called for
	statuses       map[string]string // run ID -> "running" | "done" | "failed" | "gone"
	completed      []string          // run IDs ApplyOnComplete was called for
	finished       map[string]string // run ID -> status FinishScheduleRun was called with
	checked        []string          // run IDs CheckScheduledRunStatus was called for
	createErr      map[string]error  // template ID -> error CreateFromTemplate returns
	recorded       []store.ScheduleRun
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

// CreateScheduleRun also prepends the new run to runsByTemplate (newest
// first, matching ListScheduleRunsByTemplate's contract), so a later tick
// sees it as the template's last run -- exactly what TemplateDue anchors on.
func (f *fakeSchedulingDeps) CreateScheduleRun(ctx context.Context, templateID, agentID string) (store.ScheduleRun, error) {
	run := store.ScheduleRun{
		ID: fmt.Sprintf("run_%s_%d", templateID, len(f.recorded)), TemplateID: templateID, AgentID: agentID,
		StartedAt: time.Now(), Status: store.ScheduleRunStatusRunning,
	}
	f.recorded = append(f.recorded, run)
	if f.runsByTemplate == nil {
		f.runsByTemplate = map[string][]store.ScheduleRun{}
	}
	f.runsByTemplate[templateID] = append([]store.ScheduleRun{run}, f.runsByTemplate[templateID]...)
	return run, nil
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
	if err := f.createErr[tmpl.ID]; err != nil {
		return store.Agent{}, err
	}
	return store.Agent{ID: "agt_for_" + tmpl.ID}, nil
}
func (f *fakeSchedulingDeps) CheckScheduledRunStatus(ctx context.Context, run store.ScheduleRun) (string, error) {
	f.checked = append(f.checked, run.ID)
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
		runningRuns: []store.ScheduleRun{{ID: "run_1", TemplateID: "tpl_1", AgentID: "agt_1", StartedAt: time.Now()}},
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
		runningRuns: []store.ScheduleRun{{ID: "run_1", TemplateID: "tpl_1", AgentID: "agt_1", StartedAt: time.Now()}},
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
		runningRuns: []store.ScheduleRun{{ID: "run_1", TemplateID: "tpl_1", AgentID: "agt_1", StartedAt: time.Now()}},
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
		runningRuns: []store.ScheduleRun{{ID: "run_1", TemplateID: "tpl_deleted", AgentID: "agt_1", StartedAt: time.Now()}},
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

// TestRunSchedulingTickOnce_FailedRunAppliesPolicyAndErrors pins Fix 1:
// a one-shot task whose claude exited non-zero ("failed", via the
// /workspace/.task-failed sentinel) still runs the template's cleanup
// policy, but is finalized as an error, not success.
func TestRunSchedulingTickOnce_FailedRunAppliesPolicyAndErrors(t *testing.T) {
	deps := &fakeSchedulingDeps{
		runningRuns: []store.ScheduleRun{{ID: "run_1", TemplateID: "tpl_1", AgentID: "agt_1", StartedAt: time.Now()}},
		statuses:    map[string]string{"run_1": "failed"},
		templates:   []store.Template{{ID: "tpl_1", OnComplete: "delete"}},
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
			ID: "run_1", TemplateID: "tpl_1", AgentID: "agt_1",
			StartedAt: time.Now().Add(-maxScheduledRunDuration - time.Minute),
		}},
		statuses:  map[string]string{"run_1": "running"},
		templates: []store.Template{{ID: "tpl_1", OnComplete: "stop"}},
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
// CreateFromTemplate failure still produces a ScheduleRun finalized as an
// error (visible in run history), and because that run advances
// TemplateDue's anchor, the very next tick does not retry it.
func TestRunSchedulingTickOnce_CreateFailureRecordsErrorRun(t *testing.T) {
	deps := &fakeSchedulingDeps{
		templates: []store.Template{{
			ID: "tpl_1", Enabled: true, Schedule: "0 9 * * *",
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
		createErr: map[string]error{"tpl_1": errors.New("image pull failed")},
	}
	runSchedulingTickOnce(context.Background(), deps, testLog())

	if len(deps.recorded) != 1 {
		t.Fatalf("recorded runs = %+v, want exactly one for the failed attempt", deps.recorded)
	}
	run := deps.recorded[0]
	if run.TemplateID != "tpl_1" || run.AgentID != "" {
		t.Errorf("recorded run = %+v, want TemplateID tpl_1 and an empty AgentID", run)
	}
	if deps.finished[run.ID] != store.ScheduleRunStatusError {
		t.Errorf("finished[%s] = %q, want %q", run.ID, deps.finished[run.ID], store.ScheduleRunStatusError)
	}

	runSchedulingTickOnce(context.Background(), deps, testLog())
	if len(deps.created) != 1 {
		t.Errorf("CreateFromTemplate called %d times across two ticks, want 1 (anchor advanced past the failure)", len(deps.created))
	}
}
