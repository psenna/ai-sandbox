package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// TestReconcile_RunningAgentBothStopped_WokenBackUp is the operator-restart
// scenario the feature exists for: a host/daemon restart left a StatusRunning
// agent's dind sidecar and agent container both Exited, with nobody watching
// to flip the record to stopped/error. Reconcile must bring both back and
// leave the record StatusRunning.
func TestReconcile_RunningAgentBothStopped_WokenBackUp(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	a := createRunningAgent(t, m)
	oldContainerID := a.ContainerID

	if err := f.ContainerStop(ctx, a.DindContainerID, 0); err != nil {
		t.Fatalf("simulating a stopped dind sidecar: %v", err)
	}
	if err := f.ContainerStop(ctx, a.ContainerID, 0); err != nil {
		t.Fatalf("simulating a stopped agent container: %v", err)
	}

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Woken) != 1 || rep.Woken[0] != a.ID {
		t.Fatalf("Woken = %v, want [%q]", rep.Woken, a.ID)
	}

	dind, err := f.ContainerInspect(ctx, a.DindContainerID)
	if err != nil {
		t.Fatalf("inspecting the dind sidecar: %v", err)
	}
	if dind.State != dockerclient.StateRunning {
		t.Errorf("dind State = %q, want %q", dind.State, dockerclient.StateRunning)
	}

	got, err := m.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusRunning)
	}
	if got.ContainerID == "" || got.ContainerID == oldContainerID {
		t.Errorf("ContainerID = %q (was %q), want the agent container to have been recreated", got.ContainerID, oldContainerID)
	}

	agentContainer, err := f.ContainerInspect(ctx, got.ContainerID)
	if err != nil {
		t.Fatalf("inspecting the recreated agent container: %v", err)
	}
	if agentContainer.State != dockerclient.StateRunning {
		t.Errorf("agent container State = %q, want %q", agentContainer.State, dockerclient.StateRunning)
	}

	// The Cmd wakeAgent ACTUALLY handed the daemon, read back off the fake --
	// not re-derived here. A claude-code agent must still be woken with
	// "--resume", exactly as before #180 made the flag harness-dependent.
	spec, ok := f.ContainerSpecOf(got.ContainerID)
	if !ok {
		t.Fatalf("no recorded spec for the recreated agent container %q", got.ContainerID)
	}
	if len(spec.Cmd) != 2 || spec.Cmd[0] != tmuxBootPath || spec.Cmd[1] != "--resume" {
		t.Errorf("recreated Cmd = %v, want [%q --resume] for a claude-code agent", spec.Cmd, tmuxBootPath)
	}
}

// TestWakeAgent_UsesResumeFlag proves the recreated container is told to
// resume the previous Claude Code session with "--resume" -- not "--continue"
// (Update's flag) and not a bare `claude` (Create's) -- since the old tmux
// session did not survive the container stopping.
func TestWakeAgent_UsesResumeFlag(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	a := createRunningAgent(t, m)
	spec, err := m.agentSpec(a, resolvedBackend{kind: a.Backend}, firstInvocationArgs(a, sessionResume)...)
	if err != nil {
		t.Fatalf("agentSpec: %v", err)
	}
	if len(spec.Cmd) != 2 || spec.Cmd[0] != tmuxBootPath || spec.Cmd[1] != "--resume" {
		t.Fatalf("recreated Cmd = %v, want [%q --resume]", spec.Cmd, tmuxBootPath)
	}
}

// TestWakeAgent_OpencodeUsesContinueNotResume is the safety-critical
// counterpart to TestWakeAgent_UsesResumeFlag: opencode's "--resume" exits 1
// immediately (verified live in issue #181's review), so a woken opencode
// agent must be told "--continue" instead, never "--resume". It both drives
// the real Reconcile -> wakeAgent path (proving the wake-up itself succeeds
// for an opencode agent) and pins the exact Cmd firstInvocationArgs produces
// for sessionResume, the same way TestWakeAgent_UsesResumeFlag pins it for
// claude-code.
func TestWakeAgent_OpencodeUsesContinueNotResume(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{Harness: config.HarnessOpenCode, Backend: config.BackendOllama})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Status != store.StatusRunning {
		t.Fatalf("seeded agent status = %q, want running", a.Status)
	}

	if err := f.ContainerStop(ctx, a.DindContainerID, 0); err != nil {
		t.Fatalf("simulating a stopped dind sidecar: %v", err)
	}
	if err := f.ContainerStop(ctx, a.ContainerID, 0); err != nil {
		t.Fatalf("simulating a stopped agent container: %v", err)
	}

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Woken) != 1 || rep.Woken[0] != a.ID {
		t.Fatalf("Woken = %v, want [%q]", rep.Woken, a.ID)
	}

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusRunning)
	}

	// The Cmd wakeAgent ACTUALLY handed the daemon for the recreated
	// container, read back off the fake. Deliberately NOT re-derived from
	// firstInvocationArgs here: an assertion against this test's own copy of
	// the args would keep passing even if wakeAgent went back to appending a
	// hardcoded "--resume".
	spec, ok := f.ContainerSpecOf(got.ContainerID)
	if !ok {
		t.Fatalf("no recorded spec for the recreated agent container %q", got.ContainerID)
	}
	if len(spec.Cmd) != 2 || spec.Cmd[0] != tmuxBootPath || spec.Cmd[1] != "--continue" {
		t.Fatalf("recreated Cmd = %v, want [%q --continue]", spec.Cmd, tmuxBootPath)
	}
	for _, arg := range spec.Cmd {
		if arg == "--resume" {
			t.Fatalf("recreated Cmd = %v, want no --resume for opencode (opencode --resume exits 1 immediately)", spec.Cmd)
		}
	}
}

// TestReconcile_RunningAgentAlreadyUp_NotTouched proves the "if they are
// running on a start, don't touch" rule: an agent whose containers are both
// already running across a reconcile pass keeps its exact container ID and
// is not reported as woken.
func TestReconcile_RunningAgentAlreadyUp_NotTouched(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	a := createRunningAgent(t, m)
	before := len(f.Calls())

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Woken) != 0 {
		t.Errorf("Woken = %v, want none (both containers were already running)", rep.Woken)
	}

	got, err := m.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ContainerID != a.ContainerID {
		t.Errorf("ContainerID changed to %q (was %q), want it untouched", got.ContainerID, a.ContainerID)
	}

	// Reconcile inspected the containers (findUnmanaged's listing, and this
	// agent's own inspects) but must not have stopped, started, removed or
	// recreated anything.
	for _, c := range callsAfter(f, before) {
		switch c.Op {
		case dockerclienttest.OpContainerStart, dockerclienttest.OpContainerStop,
			dockerclienttest.OpContainerRemove, dockerclienttest.OpContainerCreate:
			t.Errorf("unexpected %s on an already-running agent's containers", c.Op)
		}
	}
}

// TestReconcile_RunningAgent_DindGone_MarksError proves a resource that
// cannot be recovered by starting it (removed outright, not merely stopped)
// is reported as an error rather than silently left StatusRunning against a
// reality that does not back it up -- and that the failure does not stop the
// rest of the pass.
func TestReconcile_RunningAgent_DindGone_MarksError(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	broken := createRunningAgent(t, m)
	if err := f.ContainerRemove(ctx, broken.DindContainerID); err != nil {
		t.Fatalf("removing the dind sidecar out from under the agent: %v", err)
	}

	healthy := createRunningAgent(t, m)

	rep, err := m.Reconcile(ctx)
	if err == nil {
		t.Fatal("Reconcile: want a non-nil error naming the broken agent")
	}
	if len(rep.Woken) != 0 {
		t.Errorf("Woken = %v, want none (nothing could be recovered)", rep.Woken)
	}

	got, err := m.Get(ctx, broken.ID)
	if err != nil {
		t.Fatalf("Get(broken): %v", err)
	}
	if got.Status != store.StatusError {
		t.Errorf("broken agent Status = %q, want %q", got.Status, store.StatusError)
	}
	if got.ErrorMessage == "" {
		t.Error("broken agent ErrorMessage is empty, want an explanation")
	}
	if got.WorkspaceVolume == "" || got.ClaudeConfigVolume == "" || got.DindCacheVolume == "" {
		t.Error("broken agent's volume names were cleared, want them left exactly as they were (nothing torn down)")
	}

	// The other agent, unaffected, must still have been left alone.
	stillHealthy, err := m.Get(ctx, healthy.ID)
	if err != nil {
		t.Fatalf("Get(healthy): %v", err)
	}
	if stillHealthy.Status != store.StatusRunning || stillHealthy.ContainerID != healthy.ContainerID {
		t.Errorf("healthy agent = %+v, want it untouched", stillHealthy)
	}
}

// TestUpdate_StoppedDind_StartsItBackUp is the "when an agent goes through
// Update, check the dind container too" behaviour: Update normally never
// touches the sidecar, but a stopped one must not be left behind a freshly
// recreated agent container that boots expecting DOCKER_HOST to answer.
func TestUpdate_StoppedDind_StartsItBackUp(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	a := createRunningAgent(t, m)
	if err := f.ContainerStop(ctx, a.DindContainerID, 0); err != nil {
		t.Fatalf("stopping the dind sidecar: %v", err)
	}

	updated, err := m.Update(ctx, a.ID, UpdateRequest{})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", updated.Status, store.StatusRunning)
	}

	dind, err := f.ContainerInspect(ctx, a.DindContainerID)
	if err != nil {
		t.Fatalf("inspecting the dind sidecar: %v", err)
	}
	if dind.State != dockerclient.StateRunning {
		t.Errorf("dind State = %q, want %q (Update must have started it back up)", dind.State, dockerclient.StateRunning)
	}
}

// flakyContainerStart wraps a Fake so ContainerStart against target leaves
// the container Exited (rather than Running/healthy) for the first
// failCount calls against it, then behaves normally -- simulating the
// transient race right after a host reboot where the operator (or the
// sidecar it wakes back up) starts before the host's sysbox-runc systemd
// units have finished initializing, so the first attempt(s) to start a
// sysbox-runc container fail even though the runtime is correctly
// installed.
type flakyContainerStart struct {
	*dockerclienttest.Fake
	target    string
	failCount int
	calls     int
}

func (f *flakyContainerStart) ContainerStart(ctx context.Context, id string) error {
	if id != f.target || f.calls >= f.failCount {
		return f.Fake.ContainerStart(ctx, id)
	}
	f.calls++
	if err := f.Fake.ContainerStart(ctx, id); err != nil {
		return err
	}
	// AutoHealthy marks the container healthy the instant ContainerStart
	// runs; reset that back to "starting" (a real dind sidecar that exits
	// immediately never gets to report healthy) before stopping it, so
	// waitHealthy's Health==Healthy case does not race the State==Exited
	// case it is meant to hit.
	if err := f.SetHealth(id, dockerclient.HealthStarting); err != nil {
		return err
	}
	return f.ContainerStop(ctx, id, 0)
}

var _ dockerclient.Client = (*flakyContainerStart)(nil)

// TestReconcile_DindExitsImmediately_RetriedUntilHealthy proves
// ensureDindRunning's retry loop: a dind sidecar that exits immediately on
// its first attempts (fewer than Options.DindWakeRetries) is retried rather
// than failed outright, exactly the sysbox-runc-not-ready-yet boot race
// ensureDindRunning's doc comment describes.
func TestReconcile_DindExitsImmediately_RetriedUntilHealthy(t *testing.T) {
	f := dockerclienttest.New()
	f.AutoHealthy = true
	cfg := testConfig(5)
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(dindImage)
	f.AddImage(cfg.AgentImage)
	st := newTestStore(t, 5)
	wrapped := &flakyContainerStart{Fake: f}
	m := NewManager(wrapped, newTestRegistry(), st, cfg, testLogger(), testOptions())

	a := createRunningAgent(t, m)
	ctx := context.Background()
	if err := f.ContainerStop(ctx, a.DindContainerID, 0); err != nil {
		t.Fatalf("simulating a stopped dind sidecar: %v", err)
	}

	// Fewer failures than the configured retry budget (testOptions:
	// DindWakeRetries = 3): the sidecar must come up healthy anyway.
	wrapped.target = a.DindContainerID
	wrapped.failCount = 2

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Woken) != 1 || rep.Woken[0] != a.ID {
		t.Fatalf("Woken = %v, want [%q]", rep.Woken, a.ID)
	}

	dind, err := f.ContainerInspect(ctx, a.DindContainerID)
	if err != nil {
		t.Fatalf("inspecting the dind sidecar: %v", err)
	}
	if dind.State != dockerclient.StateRunning {
		t.Errorf("dind State = %q, want %q (should have succeeded after retrying)", dind.State, dockerclient.StateRunning)
	}

	got, err := m.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusRunning)
	}
}

// TestReconcile_DindExitsImmediately_ExhaustsRetries_RecoversViaRecreate
// proves the fallback recreateDind now takes over once the plain-start retry
// budget is exhausted: flakyContainerStart only ever targets the ORIGINAL
// container's ID, so recreateDind's brand new container (a different ID,
// same name) starts cleanly, and the agent survives StatusRunning instead of
// being marked error.
func TestReconcile_DindExitsImmediately_ExhaustsRetries_RecoversViaRecreate(t *testing.T) {
	f := dockerclienttest.New()
	f.AutoHealthy = true
	cfg := testConfig(5)
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(dindImage)
	f.AddImage(cfg.AgentImage)
	st := newTestStore(t, 5)
	wrapped := &flakyContainerStart{Fake: f}
	opts := testOptions()
	m := NewManager(wrapped, newTestRegistry(), st, cfg, testLogger(), opts)

	a := createRunningAgent(t, m)
	ctx := context.Background()
	if err := f.ContainerStop(ctx, a.DindContainerID, 0); err != nil {
		t.Fatalf("simulating a stopped dind sidecar: %v", err)
	}

	// More failures than the plain-start retry budget allows (initial
	// attempt + DindWakeRetries retries): starting the EXISTING container
	// must never succeed, forcing the fallback to recreateDind.
	wrapped.target = a.DindContainerID
	wrapped.failCount = opts.DindWakeRetries + 1

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v (recreateDind should have recovered the sidecar)", err)
	}
	if len(rep.Woken) != 1 || rep.Woken[0] != a.ID {
		t.Fatalf("Woken = %v, want [%q]", rep.Woken, a.ID)
	}

	got, err := m.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusRunning)
	}
	if got.DindContainerID == a.DindContainerID {
		t.Error("DindContainerID did not change; want a genuinely NEW container from recreateDind, not the same one")
	}
}

// TestReconcile_DindExitsImmediately_RecreateAlsoFails_MarksError proves the
// truly-unrecoverable case still ends in StatusError: when even
// recreateDind's ContainerCreate fails, Reconcile reports the failure and
// nothing is left claiming falsely to be Woken.
func TestReconcile_DindExitsImmediately_RecreateAlsoFails_MarksError(t *testing.T) {
	f := dockerclienttest.New()
	f.AutoHealthy = true
	cfg := testConfig(5)
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(dindImage)
	f.AddImage(cfg.AgentImage)
	st := newTestStore(t, 5)
	wrapped := &flakyContainerStart{Fake: f}
	opts := testOptions()
	m := NewManager(wrapped, newTestRegistry(), st, cfg, testLogger(), opts)

	a := createRunningAgent(t, m)
	ctx := context.Background()
	if err := f.ContainerStop(ctx, a.DindContainerID, 0); err != nil {
		t.Fatalf("simulating a stopped dind sidecar: %v", err)
	}

	wrapped.target = a.DindContainerID
	wrapped.failCount = opts.DindWakeRetries + 1
	f.FailOnce(dockerclienttest.OpContainerCreate, errors.New("simulated: the runtime is genuinely broken"))

	rep, err := m.Reconcile(ctx)
	if err == nil {
		t.Fatal("Reconcile: want a non-nil error, recreateDind's own ContainerCreate failed")
	}
	if len(rep.Woken) != 0 {
		t.Errorf("Woken = %v, want none", rep.Woken)
	}

	got, err := m.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusError {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusError)
	}
}

// TestReconcile_RunningDindUnhealthy_RecreatedInPlace proves the other
// recreateDind trigger: a sidecar Docker reports "running" but whose own
// healthcheck has flipped "unhealthy" (the state a sysbox-backed sidecar is
// left in when sysbox-mgr/sysbox-fs restart underneath it, e.g. after a host
// reboot) is recreated, not left alone the way a plain running-and-healthy
// sidecar is.
func TestReconcile_RunningDindUnhealthy_RecreatedInPlace(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	a := createRunningAgent(t, m)
	if err := f.SetHealth(a.DindContainerID, dockerclient.HealthUnhealthy); err != nil {
		t.Fatalf("SetHealth: %v", err)
	}

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Woken) != 1 || rep.Woken[0] != a.ID {
		t.Fatalf("Woken = %v, want [%q]", rep.Woken, a.ID)
	}

	got, err := m.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusRunning)
	}
	if got.DindContainerID == a.DindContainerID {
		t.Error("DindContainerID did not change; want a genuinely NEW container from recreateDind, not the same one")
	}

	dind, err := f.ContainerInspect(ctx, got.DindContainerID)
	if err != nil {
		t.Fatalf("inspecting the recreated dind sidecar: %v", err)
	}
	if dind.State != dockerclient.StateRunning {
		t.Errorf("recreated dind State = %q, want %q", dind.State, dockerclient.StateRunning)
	}
}

// TestUpdate_DindGone_FailsWithVolumesIntact proves Update's ensureDindRunning
// check fails loudly -- rather than recreating the agent container against a
// dead DOCKER_HOST -- when the sidecar cannot be recovered at all, and that
// the failure leaves the agent retryable (StatusError, volumes intact).
func TestUpdate_DindGone_FailsWithVolumesIntact(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	a := createRunningAgent(t, m)
	if err := f.ContainerRemove(ctx, a.DindContainerID); err != nil {
		t.Fatalf("removing the dind sidecar: %v", err)
	}

	_, err := m.Update(ctx, a.ID, UpdateRequest{})
	if err == nil {
		t.Fatal("Update: want a non-nil error, the dind sidecar is gone")
	}

	got, err := m.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusError {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusError)
	}
	if got.WorkspaceVolume == "" || got.ClaudeConfigVolume == "" || got.DindCacheVolume == "" {
		t.Error("volume names were cleared, want them left exactly as they were (nothing torn down)")
	}
	// The OLD agent container must not have been touched (still there,
	// unchanged) since ensureDindRunning runs before removeAgentContainer.
	agentContainer, err := f.ContainerInspect(ctx, a.ContainerID)
	if err != nil {
		t.Fatalf("inspecting the untouched agent container: %v", err)
	}
	if agentContainer.State != dockerclient.StateRunning {
		t.Errorf("agent container State = %q, want %q (Update must not have removed it before the dind check failed)", agentContainer.State, dockerclient.StateRunning)
	}
}

// TestReconcile_DependaproxyRecreated_ResyncsRunningAgent proves #169's core
// scenario end to end: the shared dependaproxy container gets recreated
// (losing every agent's dinernet attachment) while an agent's own containers
// stay running throughout, and a routine Reconcile pass -- via wakeAgent's
// "already running" branch -- notices the drift, reconnects, re-stamps the
// record, and refreshes /workspace/dependaproxy-ip inside the still-running
// agent container.
func TestReconcile_DependaproxyRecreated_ResyncsRunningAgent(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	a := createRunningAgent(t, m)
	oldAddr := a.DependaproxyDinernetIP

	// Simulate the shared dependaproxy container being recreated: the OLD
	// container (and its dinernet endpoint) is gone, a NEW one with the same
	// name takes its place, attached to nothing.
	if err := f.ContainerRemove(ctx, m.cfg.DependaproxyContainer); err != nil {
		t.Fatalf("removing the shared dependaproxy container: %v", err)
	}
	newDependaproxy(t, f, m.cfg.DependaproxyContainer)

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Woken) != 0 {
		t.Errorf("Woken = %v, want none (both containers were already running throughout)", rep.Woken)
	}

	got, err := m.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusRunning)
	}
	if !got.DependaproxyDinernetIP.IsValid() || got.DependaproxyDinernetIP == oldAddr {
		t.Errorf("DependaproxyDinernetIP = %v, want a freshly assigned address (old was %v)", got.DependaproxyDinernetIP, oldAddr)
	}

	// The running agent container's dependaproxy-ip file must have been
	// refreshed via an exec against it (not a recreate: the container was
	// never stopped or removed).
	if hasCall(f.Calls(), dockerclienttest.OpContainerRemove, a.ContainerID) {
		t.Error("the agent container was recreated; want it left running and refreshed via exec instead")
	}
	foundExec := false
	for _, c := range f.Calls() {
		if c.Op == dockerclienttest.OpExecCreate && c.Target == a.ContainerID {
			foundExec = true
		}
	}
	if !foundExec {
		t.Errorf("Calls() = %v, want an ExecCreate against the agent container %q (refreshDependaproxyIPFile)", f.Calls(), a.ContainerID)
	}
}
