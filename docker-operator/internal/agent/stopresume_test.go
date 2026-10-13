package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// cancelAfterNthContainerStop wraps a dockerclient.Client and cancels a
// context.CancelFunc right after the Nth successful ContainerStop call --
// used to simulate the caller's request context being cancelled at the exact
// moment StopAgent has finished stopping both containers and is about to
// make its final store write (markStopped). This fails that write with
// context.Canceled while leaving the record (and both now-stopped
// containers) completely intact, which is precisely the scenario
// TestStopAgent_MarkStoppedFailureLandsError exercises.
type cancelAfterNthContainerStop struct {
	dockerclient.Client
	n      int
	cancel context.CancelFunc

	mu    sync.Mutex
	count int
}

func (c *cancelAfterNthContainerStop) ContainerStop(ctx context.Context, id string, timeout time.Duration) error {
	err := c.Client.ContainerStop(ctx, id, timeout)
	if err == nil {
		c.mu.Lock()
		c.count++
		if c.count == c.n {
			c.cancel()
		}
		c.mu.Unlock()
	}
	return err
}

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

// TestStopAgent_MarkStoppedFailureLandsError is a Review Focus case: if
// BOTH ContainerStop calls succeed but the final store write that lands
// StatusStopped (markStopped) itself fails -- here because the caller's
// context is cancelled at exactly that moment, simulated via
// cancelAfterNthContainerStop -- the record must still land StatusError via
// markStopError, not stay stuck in StatusUpdating (the shield state). This
// mirrors TestStopAgent_PartialFailureLandsError but exercises the LAST
// branch of StopAgent rather than the ContainerStop-failure branch.
func TestStopAgent_MarkStoppedFailureLandsError(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	a := createRunningAgent(t, m)

	ctx, cancel := context.WithCancel(context.Background())
	m.docker = &cancelAfterNthContainerStop{Client: f, n: 2, cancel: cancel}

	if _, err := m.StopAgent(ctx, a.ID); err == nil {
		t.Fatal("StopAgent = nil error, want the cancellation to surface as an error")
	}

	got, err := st.Get(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusError {
		t.Errorf("Status = %q, want %q (must not be left stuck in %q)", got.Status, store.StatusError, store.StatusUpdating)
	}
	if got.ContainerID == "" {
		t.Error("ContainerID cleared, want it left intact -- Stop never removes anything")
	}

	c, err := f.ContainerInspect(context.Background(), a.ContainerID)
	if err != nil {
		t.Fatalf("ContainerInspect(agent): %v", err)
	}
	if c.State != dockerclient.StateExited {
		t.Errorf("agent container state = %v, want exited (both stops had already succeeded)", c.State)
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
