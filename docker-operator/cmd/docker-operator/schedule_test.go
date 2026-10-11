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
	cronAgents      []store.CronAgent
	templates       map[string]store.Template
	runsByCronAgent map[string][]store.ScheduleRun
	runningRuns     []store.ScheduleRun
	created         []string          // cron agent IDs CreateFromCronAgent was called for
	statuses        map[string]string // run ID -> "running" | "done" | "failed" | "gone"
	completed       []string          // run IDs ApplyOnComplete was called for
	finished        map[string]string // run ID -> status FinishScheduleRun was called with
	checked         []string          // run IDs CheckScheduledRunStatus was called for
	createErr       map[string]error  // cron agent ID -> error CreateFromCronAgent returns
	recorded        []store.ScheduleRun
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
