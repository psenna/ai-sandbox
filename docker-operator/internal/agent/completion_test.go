package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// newTestCronAgent creates a Template with the given harness/backend and
// returns a CronAgent referencing it with the given task/OnComplete.
// CreateFromCronAgent resolves TemplateID to a real Template record, so
// these tests can no longer build a bare store.CronAgent in isolation the
// way TestCreateFromTemplate once built a bare store.Template.
func newTestCronAgent(t *testing.T, m *Manager, harness, backend, task, onComplete string) store.CronAgent {
	t.Helper()
	tmpl, err := m.CreateTemplate(context.Background(), store.TemplateCreateSpec{Harness: harness, Backend: backend})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	return store.CronAgent{TemplateID: tmpl.ID, Task: task, OnComplete: onComplete}
}

func TestCreateFromCronAgent(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	ctx := context.Background()

	ca := newTestCronAgent(t, m, config.HarnessClaudeCode, config.BackendOllama, "do the daily thing", "")
	a, err := m.CreateFromCronAgent(ctx, ca)
	if err != nil {
		t.Fatalf("CreateFromCronAgent: %v", err)
	}
	if a.Harness != config.HarnessClaudeCode {
		t.Errorf("Harness = %q, want %q", a.Harness, config.HarnessClaudeCode)
	}
}

func TestCheckScheduledRunStatus_NotDoneYet(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	ca := newTestCronAgent(t, m, config.HarnessClaudeCode, config.BackendOllama, "x", "")
	a, err := m.CreateFromCronAgent(ctx, ca)
	if err != nil {
		t.Fatalf("CreateFromCronAgent: %v", err)
	}
	f.ExecOutput[strings.Join(scheduledRunCheckCmd, " ")] = nil

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

	ca := newTestCronAgent(t, m, config.HarnessClaudeCode, config.BackendOllama, "x", "")
	a, err := m.CreateFromCronAgent(ctx, ca)
	if err != nil {
		t.Fatalf("CreateFromCronAgent: %v", err)
	}
	f.ExecOutput[strings.Join(scheduledRunCheckCmd, " ")] = []byte("complete\n")

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

// TestCheckScheduledRunStatus_Failed pins Fix 1: the failure sentinel
// /workspace/.task-failed (written by claude-supervisor.sh on a non-zero
// one-shot exit) reports "failed".
func TestCheckScheduledRunStatus_Failed(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	ca := newTestCronAgent(t, m, config.HarnessClaudeCode, config.BackendOllama, "x", "")
	a, err := m.CreateFromCronAgent(ctx, ca)
	if err != nil {
		t.Fatalf("CreateFromCronAgent: %v", err)
	}
	f.ExecOutput[strings.Join(scheduledRunCheckCmd, " ")] = []byte("failed\n")

	status, err := m.CheckScheduledRunStatus(ctx, store.ScheduleRun{AgentID: a.ID})
	if err != nil {
		t.Fatalf("CheckScheduledRunStatus: %v", err)
	}
	if status != "failed" {
		t.Errorf("status = %q, want %q (failure sentinel present)", status, "failed")
	}
}

// TestCheckScheduledRunStatus_StoppedContainerIsGone pins Fix 2: a
// container that still exists but is not running rejects exec with a
// non-not-found error (a 409 conflict on a real daemon); that must report
// "gone", not error on every tick forever.
func TestCheckScheduledRunStatus_StoppedContainerIsGone(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	ca := newTestCronAgent(t, m, config.HarnessClaudeCode, config.BackendOllama, "x", "")
	a, err := m.CreateFromCronAgent(ctx, ca)
	if err != nil {
		t.Fatalf("CreateFromCronAgent: %v", err)
	}
	if err := f.ContainerStop(ctx, agentContainerName(a.ID), 0); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	// The fake does not itself reject exec on a stopped container; inject
	// the daemon's conflict error the way a real one would answer.
	f.FailOnce(dockerclienttest.OpExecCreate, fmt.Errorf("container %s is not running", a.ID))

	status, err := m.CheckScheduledRunStatus(ctx, store.ScheduleRun{AgentID: a.ID})
	if err != nil {
		t.Fatalf("CheckScheduledRunStatus(stopped): %v, want nil error", err)
	}
	if status != "gone" {
		t.Errorf("status = %q, want %q for a stopped container", status, "gone")
	}
}

// TestCheckScheduledRunStatus_RunningExecErrorPropagates is Fix 2's other
// half: if the container really is running and the exec still failed, the
// error is real and is returned (retried next tick), not masked as "gone".
func TestCheckScheduledRunStatus_RunningExecErrorPropagates(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	ca := newTestCronAgent(t, m, config.HarnessClaudeCode, config.BackendOllama, "x", "")
	a, err := m.CreateFromCronAgent(ctx, ca)
	if err != nil {
		t.Fatalf("CreateFromCronAgent: %v", err)
	}
	boom := errors.New("daemon hiccup")
	f.FailOnce(dockerclienttest.OpExecCreate, boom)

	status, err := m.CheckScheduledRunStatus(ctx, store.ScheduleRun{AgentID: a.ID})
	if !errors.Is(err, boom) {
		t.Errorf("CheckScheduledRunStatus = (%q, %v), want the exec error", status, err)
	}
}

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
		t.Fatalf("ContainerInspect: %v", err)
	}
	if c.State != dockerclient.StateExited {
		t.Errorf("container state = %v, want exited (stopped)", c.State)
	}
}

// TestApplyOnComplete_StopShieldsRecord pins Fix 4: the "stop" policy
// marks the record StatusStopped BEFORE stopping the container, so the
// container's `die` event (MarkUnexpectedExit only flips a StatusRunning
// record) cannot turn a deliberate stop into a false StatusError.
func TestApplyOnComplete_StopShieldsRecord(t *testing.T) {
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
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusStopped)
	}
}

func TestApplyOnComplete_Delete(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()

	ca := newTestCronAgent(t, m, config.HarnessClaudeCode, config.BackendOllama, "x", "delete")
	a, err := m.CreateFromCronAgent(ctx, ca)
	if err != nil {
		t.Fatalf("CreateFromCronAgent: %v", err)
	}
	run := store.ScheduleRun{AgentID: a.ID}
	if err := m.ApplyOnComplete(ctx, ca, run); err != nil {
		t.Fatalf("ApplyOnComplete: %v", err)
	}

	if _, err := st.Get(ctx, a.ID); !store.IsNotFound(err) {
		t.Errorf("agent record still present after OnComplete delete: %v", err)
	}
}

func TestApplyOnComplete_Leave(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	ca := newTestCronAgent(t, m, config.HarnessClaudeCode, config.BackendOllama, "x", "leave")
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
		t.Fatalf("ContainerInspect: %v", err)
	}
	if c.State != dockerclient.StateRunning {
		t.Errorf("container state = %v, want still running (leave)", c.State)
	}
}

func TestListScheduleRunsByCronAgent(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()

	if _, err := st.CreateScheduleRun(ctx, "ca_1", "agt_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateScheduleRun(ctx, "ca_2", "agt_2"); err != nil {
		t.Fatal(err)
	}

	got, err := m.ListScheduleRunsByCronAgent(ctx, "ca_1")
	if err != nil {
		t.Fatalf("ListScheduleRunsByCronAgent: %v", err)
	}
	if len(got) != 1 || got[0].CronAgentID != "ca_1" {
		t.Errorf("ListScheduleRunsByCronAgent(ca_1) = %+v, want exactly ca_1's run", got)
	}
}
