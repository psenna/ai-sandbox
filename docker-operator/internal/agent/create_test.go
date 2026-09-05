package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// wantNames is the set of resource names Create is expected to derive for
// one agent ID, computed once and shared by TestCreate_Success's checks.
type wantNames struct {
	container, dind, dinernet, workspace, claudeConfig, dindCache string
}

func namesFor(id string) wantNames {
	container := "docker-operator-agent-" + id
	return wantNames{
		container:    container,
		dind:         "docker-operator-dind-" + id,
		dinernet:     container + "-dinernet",
		workspace:    container + "-workspace",
		claudeConfig: container + "-claude-config",
		dindCache:    container + "-dind-cache",
	}
}

// TestCreate_Success exercises the entire happy path: resource names, the
// #69 Cmd-wiring proof, labels on every resource, the exact Docker call
// order, and the final StatusRunning record.
func TestCreate_Success(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()
	before := len(f.Calls()) // newTestManager's dependaproxy seeding already made some calls

	got, err := m.Create(ctx, CreateRequest{Name: "test-agent", Description: "a test agent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := namesFor(got.ID)

	checkCreateRecord(t, got, want)
	checkCreateLabels(t, f, got, want)
	checkCmdWiring(t, m, got)
	checkCreateCallOrder(t, f, m, got, want, before)
}

func checkCreateRecord(t *testing.T, got store.Agent, want wantNames) {
	t.Helper()
	if got.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusRunning)
	}
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage = %q, want empty", got.ErrorMessage)
	}
	if got.Name != "test-agent" || got.Description != "a test agent" {
		t.Errorf("Name/Description = %q/%q, want the request's values", got.Name, got.Description)
	}
	for _, c := range []struct{ name, got, want string }{
		{"ContainerName", got.ContainerName, want.container},
		{"DindContainerName", got.DindContainerName, want.dind},
		{"DinernetName", got.DinernetName, want.dinernet},
		{"WorkspaceVolume", got.WorkspaceVolume, want.workspace},
		{"ClaudeConfigVolume", got.ClaudeConfigVolume, want.claudeConfig},
		{"DindCacheVolume", got.DindCacheVolume, want.dindCache},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if got.ContainerID == "" {
		t.Error("ContainerID is empty, want it stamped")
	}
	if got.DindContainerID == "" {
		t.Error("DindContainerID is empty, want it stamped")
	}
	if got.DinernetID == "" {
		t.Error("DinernetID is empty, want it stamped")
	}
	if !got.DependaproxyDinernetIP.IsValid() {
		t.Error("DependaproxyDinernetIP is not valid, want it stamped from NetworkConnect")
	}
}

// checkCreateLabels proves every resource this create attempt owns carries
// all three ai-sandbox.docker-operator/* labels.
func checkCreateLabels(t *testing.T, f *dockerclienttest.Fake, got store.Agent, want wantNames) {
	t.Helper()
	for _, v := range f.Volumes() {
		if v.Labels[LabelManaged] != LabelManagedValue || v.Labels[LabelAgentID] != got.ID {
			t.Errorf("volume %q labels = %v, want managed=true and agent-id=%q", v.Name, v.Labels, got.ID)
		}
	}
	for _, n := range f.Networks() {
		if n.Name != want.dinernet {
			continue
		}
		if n.Labels[LabelManaged] != LabelManagedValue || n.Labels[LabelAgentID] != got.ID || n.Labels[LabelRole] != string(RoleDinernet) {
			t.Errorf("network %q labels = %v", n.Name, n.Labels)
		}
	}
	for _, c := range f.Containers() {
		var wantRole Role
		switch c.Name {
		case want.container:
			wantRole = RoleAgent
		case want.dind:
			wantRole = RoleDind
		default:
			continue // the shared dependaproxy container, unrelated to this agent
		}
		if c.Labels[LabelManaged] != LabelManagedValue || c.Labels[LabelAgentID] != got.ID || c.Labels[LabelRole] != string(wantRole) {
			t.Errorf("container %q labels = %v, want role %q", c.Name, c.Labels, wantRole)
		}
	}
}

// checkCmdWiring is #69's Cmd-wiring proof. dockerclienttest.Fake does not
// expose a created container's full spec through its public API, so the
// spec-building methods are checked directly -- same package, same values
// Create used.
func checkCmdWiring(t *testing.T, m *Manager, got store.Agent) {
	t.Helper()
	spec := m.agentSpec(got, resolvedBackend{
		kind:      config.BackendOllama,
		model:     m.cfg.AgentModel,
		fastModel: m.cfg.AgentFastModel,
	})
	if len(spec.Cmd) != 1 || spec.Cmd[0] != tmuxBootPath {
		t.Errorf("agent ContainerSpec.Cmd = %v, want [%q]", spec.Cmd, tmuxBootPath)
	}
	if spec.Entrypoint != nil {
		t.Errorf("agent ContainerSpec.Entrypoint = %v, want nil (the image's own ENTRYPOINT must run)", spec.Entrypoint)
	}
	dspec := m.dindSpec(got)
	if dspec.Runtime != m.cfg.DockerRuntime {
		t.Errorf("dind ContainerSpec.Runtime = %q, want %q", dspec.Runtime, m.cfg.DockerRuntime)
	}
	if dspec.Healthcheck == nil {
		t.Error("dind ContainerSpec.Healthcheck is nil, want one declared")
	}
}

// checkCreateCallOrder proves the exact Docker call order, up through the
// agent container starting, then the trailing tmux-session-check exec calls
// (whose exec ID is only known after ExecCreate runs).
func checkCreateCallOrder(t *testing.T, f *dockerclienttest.Fake, m *Manager, got store.Agent, want wantNames, before int) {
	t.Helper()
	wantPrefix := []dockerclienttest.Call{
		{Op: dockerclienttest.OpImageInspect, Target: dindImage},
		{Op: dockerclienttest.OpImageInspect, Target: m.cfg.AgentImage},
		{Op: dockerclienttest.OpVolumeCreate, Target: want.workspace},
		{Op: dockerclienttest.OpVolumeCreate, Target: want.claudeConfig},
		{Op: dockerclienttest.OpVolumeCreate, Target: want.dindCache},
		{Op: dockerclienttest.OpNetworkCreate, Target: want.dinernet},
		{Op: dockerclienttest.OpContainerCreate, Target: want.dind},
		{Op: dockerclienttest.OpContainerStart, Target: got.DindContainerID},
		{Op: dockerclienttest.OpContainerInspect, Target: got.DindContainerID},
		{Op: dockerclienttest.OpNetworkConnect, Target: want.dinernet + "/" + m.cfg.DependaproxyContainer},
		{Op: dockerclienttest.OpContainerCreate, Target: want.container},
		{Op: dockerclienttest.OpContainerStart, Target: got.ContainerID},
	}
	calls := f.Calls()[before:]
	if len(calls) < len(wantPrefix)+3 {
		t.Fatalf("Calls() (since Create was invoked) = %v, want at least %d calls", calls, len(wantPrefix)+3)
	}
	for i, want := range wantPrefix {
		if calls[i] != want {
			t.Errorf("Calls()[%d] = %+v, want %+v", i, calls[i], want)
		}
	}
	// The tmux-session check: ExecCreate, then ExecAttach/ExecInspect share
	// whatever exec ID ExecCreate produced.
	tail := calls[len(wantPrefix):]
	if len(tail) != 3 {
		t.Fatalf("trailing calls = %v, want exactly [ExecCreate, ExecAttach, ExecInspect]", tail)
	}
	if tail[0].Op != dockerclienttest.OpExecCreate || tail[0].Target != got.ContainerID {
		t.Errorf("Calls()[%d] = %+v, want {ExecCreate, %q}", len(wantPrefix), tail[0], got.ContainerID)
	}
	if tail[1].Op != dockerclienttest.OpExecAttach || tail[2].Op != dockerclienttest.OpExecInspect {
		t.Errorf("trailing calls = %v, want [ExecCreate, ExecAttach, ExecInspect]", tail)
	}
	if tail[1].Target == "" || tail[1].Target != tail[2].Target {
		t.Errorf("ExecAttach/ExecInspect targets = %q/%q, want the same non-empty exec ID", tail[1].Target, tail[2].Target)
	}
}

// TestCreate_AtCapacity proves a create over MAX_AGENTS is rejected before it
// ever reaches Docker.
func TestCreate_AtCapacity(t *testing.T) {
	m, f, st := newTestManager(t, 1)
	ctx := context.Background()

	if _, err := st.Create(ctx, store.CreateSpec{ID: "agt_prefill"}); err != nil {
		t.Fatalf("prefilling the one slot: %v", err)
	}
	before := len(f.Calls())

	_, err := m.Create(ctx, CreateRequest{})
	if err == nil {
		t.Fatal("Create at capacity = nil error, want one satisfying store.IsAtCapacity")
	}
	if !store.IsAtCapacity(err) {
		t.Errorf("Create error = %v, want store.IsAtCapacity", err)
	}
	if after := len(f.Calls()); after != before {
		t.Errorf("Create at capacity made %d new docker calls, want 0: %v", after-before, f.Calls()[before:])
	}
}

// TestCreate_ImagePullOnlyWhenMissing proves ensureImage only pulls the image
// that was actually absent from the daemon.
func TestCreate_ImagePullOnlyWhenMissing(t *testing.T) {
	f := dockerclienttest.New()
	f.AutoHealthy = true
	cfg := testConfig(5)
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(cfg.AgentImage) // only the agent image is pre-seeded; dindImage is missing
	st := newTestStore(t, 5)
	m := NewManager(f, st, cfg, testLogger(), testOptions())

	if _, err := m.Create(context.Background(), CreateRequest{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var pulledDind, pulledAgent bool
	for _, c := range f.Calls() {
		if c.Op != dockerclienttest.OpImagePull {
			continue
		}
		switch c.Target {
		case dindImage:
			pulledDind = true
		case cfg.AgentImage:
			pulledAgent = true
		}
	}
	if !pulledDind {
		t.Error("the dind image was not pulled despite being missing from the daemon")
	}
	if pulledAgent {
		t.Error("the agent image was pulled despite already being present on the daemon")
	}
}

// TestCreate_RollbackOnFailure injects a failure at each major create step
// and asserts the daemon and the store both end up exactly as they started:
// zero leftover Docker resources, zero leftover store records (the slot is
// given back).
func TestCreate_RollbackOnFailure(t *testing.T) {
	cases := []struct {
		name string
		fail func(f *dockerclienttest.Fake)
	}{
		{"volume", func(f *dockerclienttest.Fake) {
			f.FailOnce(dockerclienttest.OpVolumeCreate, errors.New("boom: volume create"))
		}},
		{"network", func(f *dockerclienttest.Fake) {
			f.FailOnce(dockerclienttest.OpNetworkCreate, errors.New("boom: network create"))
		}},
		{"container-create", func(f *dockerclienttest.Fake) {
			f.FailOnce(dockerclienttest.OpContainerCreate, errors.New("boom: container create"))
		}},
		{"container-start", func(f *dockerclienttest.Fake) {
			f.FailOnce(dockerclienttest.OpContainerStart, errors.New("boom: container start"))
		}},
		{"network-connect", func(f *dockerclienttest.Fake) {
			f.FailOnce(dockerclienttest.OpNetworkConnect, errors.New("boom: network connect"))
		}},
		{"exec-create", func(f *dockerclienttest.Fake) {
			// waitTmuxSession treats one exec-create error as transient and
			// retries (that is what lets a slow-starting daemon recover), so
			// a single FailOnce would just be absorbed by the next poll --
			// this must fail STICKILY, until the tmux-ready timeout gives up.
			f.Fail(dockerclienttest.OpExecCreate, errors.New("boom: exec create"))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, f, st := newTestManager(t, 5)
			before := snapshotCounts(f)

			tc.fail(f)

			_, err := m.Create(context.Background(), CreateRequest{})
			if err == nil {
				t.Fatal("Create = nil error, want the injected failure to propagate")
			}

			if after := snapshotCounts(f); after != before {
				t.Errorf("docker resources after rollback = %+v, want back to baseline %+v", after, before)
			}
			agents, lerr := st.List(context.Background())
			if lerr != nil {
				t.Fatalf("List: %v", lerr)
			}
			if len(agents) != 0 {
				t.Errorf("store records after rollback = %v, want none (the slot must be freed)", agents)
			}
		})
	}
}

// TestCreate_DindNeverHealthy_TimeoutRollback proves a sidecar that never
// reports healthy times out and rolls back cleanly, rather than hanging or
// leaking resources.
func TestCreate_DindNeverHealthy_TimeoutRollback(t *testing.T) {
	f := dockerclienttest.New() // AutoHealthy left false deliberately: health never advances
	cfg := testConfig(5)
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(dindImage)
	f.AddImage(cfg.AgentImage)
	st := newTestStore(t, 5)
	m := NewManager(f, st, cfg, testLogger(), testOptions())

	before := snapshotCounts(f)
	_, err := m.Create(context.Background(), CreateRequest{})
	if err == nil {
		t.Fatal("Create = nil error, want a dind-health timeout")
	}
	if !strings.Contains(err.Error(), "did not become healthy") {
		t.Errorf("Create error = %v, want it to mention the health timeout", err)
	}
	if after := snapshotCounts(f); after != before {
		t.Errorf("docker resources after rollback = %+v, want back to baseline %+v", after, before)
	}
	if agents, _ := st.List(context.Background()); len(agents) != 0 {
		t.Errorf("store records after rollback = %v, want none", agents)
	}
}

// TestCreate_TmuxNeverAppears_TimeoutRollback proves an agent container whose
// tmux session never comes up (tmux-boot.sh failed, or claude crashed the
// pane before remain-on-exit even mattered) times out and rolls back cleanly.
func TestCreate_TmuxNeverAppears_TimeoutRollback(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	f.ExecExit["tmux has-session -t main"] = 1 // non-zero: the session never appears

	before := snapshotCounts(f)
	_, err := m.Create(context.Background(), CreateRequest{})
	if err == nil {
		t.Fatal("Create = nil error, want a tmux-session timeout")
	}
	if !strings.Contains(err.Error(), "did not appear") {
		t.Errorf("Create error = %v, want it to mention the tmux session not appearing", err)
	}
	if after := snapshotCounts(f); after != before {
		t.Errorf("docker resources after rollback = %+v, want back to baseline %+v", after, before)
	}
	if agents, _ := st.List(context.Background()); len(agents) != 0 {
		t.Errorf("store records after rollback = %v, want none", agents)
	}
}

// cancelOnContainerCreate wraps a Fake so its first ContainerCreate call both
// fails and cancels the context Create was called with, letting
// TestCreate_RollbackSurvivesCancelledContext prove rollback still runs to
// completion under a caller context that is already cancelled by the time it
// executes.
type cancelOnContainerCreate struct {
	*dockerclienttest.Fake
	cancel    context.CancelFunc
	triggered bool
}

func (c *cancelOnContainerCreate) ContainerCreate(ctx context.Context, spec dockerclient.ContainerSpec) (string, error) {
	if !c.triggered {
		c.triggered = true
		c.cancel()
		return "", errors.New("forced container-create failure to test rollback under a cancelled context")
	}
	return c.Fake.ContainerCreate(ctx, spec)
}

var _ dockerclient.Client = (*cancelOnContainerCreate)(nil)

// TestCreate_RollbackSurvivesCancelledContext proves rollback's use of
// context.WithoutCancel matters: if rollback re-used the caller's (by then
// cancelled) context for its own store.Delete, that delete would itself fail
// with context.Canceled, stranding the record and its MAX_AGENTS slot.
func TestCreate_RollbackSurvivesCancelledContext(t *testing.T) {
	f := dockerclienttest.New()
	f.AutoHealthy = true
	cfg := testConfig(5)
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(dindImage)
	f.AddImage(cfg.AgentImage)
	st := newTestStore(t, 5)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapped := &cancelOnContainerCreate{Fake: f, cancel: cancel}
	m := NewManager(wrapped, st, cfg, testLogger(), testOptions())

	before := snapshotCounts(f)
	_, err := m.Create(ctx, CreateRequest{})
	if err == nil {
		t.Fatal("Create = nil error, want the injected container-create failure")
	}
	if ctx.Err() == nil {
		t.Fatal("test bug: the context was never actually cancelled")
	}

	// Checked against a FRESH, uncancelled context and Fake calls that ignore
	// context cancellation entirely -- the only thing that could distinguish
	// "rollback used context.WithoutCancel" from "it didn't" is whether
	// store.Delete (which DOES check ctx.Err()) succeeded.
	agents, lerr := st.List(context.Background())
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(agents) != 0 {
		t.Errorf("store records after rollback under a cancelled context = %v, want none (slot freed)", agents)
	}
	if after := snapshotCounts(f); after != before {
		t.Errorf("docker resources after rollback under a cancelled context = %+v, want back to baseline %+v", after, before)
	}
}

// A short sanity check that Options.withDefaults actually fills in every
// field, since every timeout test above depends on it never leaving one at
// its zero value (which would mean "no timeout", not "fast timeout").
func TestOptions_WithDefaults(t *testing.T) {
	o := Options{}.withDefaults()
	for _, d := range []time.Duration{
		o.DindHealthTimeout, o.TmuxReadyTimeout, o.ExecTimeout,
		o.PollInterval, o.StopTimeout, o.TeardownTimeout,
	} {
		if d <= 0 {
			t.Errorf("Options{}.withDefaults() left a field at %s, want > 0", d)
		}
	}
}
