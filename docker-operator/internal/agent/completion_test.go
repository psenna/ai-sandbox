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
	if c.State != dockerclient.StateExited {
		t.Errorf("container state = %v, want exited (stopped)", c.State)
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
