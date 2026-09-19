package agent

import (
	"context"
	"errors"
	"net/netip"
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
	spec, err := m.agentSpec(got, resolvedBackend{
		kind:      config.BackendOllama,
		model:     m.cfg.AgentModel,
		fastModel: m.cfg.AgentFastModel,
	})
	if err != nil {
		t.Fatalf("agentSpec: %v", err)
	}
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
		// stampImageID's own inspect, right after ensureImages, to record
		// store.Agent.ImageID.
		{Op: dockerclienttest.OpImageInspect, Target: m.cfg.AgentImage},
		{Op: dockerclienttest.OpVolumeCreate, Target: want.workspace},
		{Op: dockerclienttest.OpVolumeCreate, Target: want.claudeConfig},
		{Op: dockerclienttest.OpVolumeCreate, Target: want.dindCache},
		{Op: dockerclienttest.OpNetworkCreate, Target: want.dinernet},
		{Op: dockerclienttest.OpContainerCreate, Target: want.dind},
		{Op: dockerclienttest.OpContainerStart, Target: got.DindContainerID},
		{Op: dockerclienttest.OpContainerInspect, Target: got.DindContainerID},
		{Op: dockerclienttest.OpNetworkConnect, Target: want.dinernet + "/" + m.cfg.DependaproxyContainer},
	}
	calls := f.Calls()[before:]
	if len(calls) < len(wantPrefix)+3+2+3 {
		t.Fatalf("Calls() (since Create was invoked) = %v, want at least %d calls", calls, len(wantPrefix)+3+2+3)
	}
	for i, want := range wantPrefix {
		if calls[i] != want {
			t.Errorf("Calls()[%d] = %+v, want %+v", i, calls[i], want)
		}
	}
	rest := calls[len(wantPrefix):]

	// verifyDependaproxyReachable's own check, right after NetworkConnect:
	// ExecCreate against the DIND container, then ExecAttach/ExecInspect
	// sharing whatever exec ID it produced.
	rest = checkExecTriplet(t, rest, got.DindContainerID, "verifyDependaproxyReachable")

	wantSuffix := []dockerclienttest.Call{
		{Op: dockerclienttest.OpContainerCreate, Target: want.container},
		{Op: dockerclienttest.OpContainerStart, Target: got.ContainerID},
	}
	for i, want := range wantSuffix {
		if rest[i] != want {
			t.Errorf("Calls()[%d] = %+v, want %+v", len(wantPrefix)+3+i, rest[i], want)
		}
	}
	rest = rest[len(wantSuffix):]

	// The tmux-session check: same ExecCreate/ExecAttach/ExecInspect shape,
	// this time against the agent container.
	rest = checkExecTriplet(t, rest, got.ContainerID, "the tmux-session check")
	if len(rest) != 0 {
		t.Errorf("trailing calls = %v, want none", rest)
	}
}

// checkExecTriplet asserts the next three calls are the ExecCreate/ExecAttach/
// ExecInspect shape runExec always produces against wantContainerID, and
// returns whatever calls remain after them.
func checkExecTriplet(t *testing.T, calls []dockerclienttest.Call, wantContainerID, what string) []dockerclienttest.Call {
	t.Helper()
	if len(calls) < 3 {
		t.Fatalf("%s: calls = %v, want at least [ExecCreate, ExecAttach, ExecInspect]", what, calls)
	}
	triplet, rest := calls[:3], calls[3:]
	if triplet[0].Op != dockerclienttest.OpExecCreate || triplet[0].Target != wantContainerID {
		t.Errorf("%s: Calls()[0] = %+v, want {ExecCreate, %q}", what, triplet[0], wantContainerID)
	}
	if triplet[1].Op != dockerclienttest.OpExecAttach || triplet[2].Op != dockerclienttest.OpExecInspect {
		t.Errorf("%s: calls = %v, want [ExecCreate, ExecAttach, ExecInspect]", what, triplet)
	}
	if triplet[1].Target == "" || triplet[1].Target != triplet[2].Target {
		t.Errorf("%s: ExecAttach/ExecInspect targets = %q/%q, want the same non-empty exec ID", what, triplet[1].Target, triplet[2].Target)
	}
	return rest
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
	m := NewManager(f, nil, st, cfg, testLogger(), testOptions())

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
	m := NewManager(f, nil, st, cfg, testLogger(), testOptions())

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
	m := NewManager(wrapped, nil, st, cfg, testLogger(), testOptions())

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

func TestAutoModeArgs(t *testing.T) {
	if got := autoModeArgs(store.Agent{AutoMode: config.AutoModeOn}); len(got) != 2 || got[0] != "--permission-mode" || got[1] != "auto" {
		t.Errorf("autoModeArgs(on) = %v, want [--permission-mode auto]", got)
	}
	if got := autoModeArgs(store.Agent{AutoMode: config.AutoModeOff}); got != nil {
		t.Errorf("autoModeArgs(off) = %v, want nil", got)
	}
	if got := autoModeArgs(store.Agent{}); got != nil {
		t.Errorf("autoModeArgs(unset, a pre-feature record) = %v, want nil", got)
	}
}

// TestCreate_AutoMode_ResolvesAndValidates covers the three shapes a create
// request's AutoMode can take: no override (inherits the operator's
// DefaultAutoMode), an explicit override, and an invalid value (rejected
// before a slot is reserved, like every other create-form field).
func TestCreate_AutoMode_ResolvesAndValidates(t *testing.T) {
	cfg := testConfig(5)
	cfg.DefaultAutoMode = true
	m, _, st := newTestManagerCfg(t, cfg)
	ctx := context.Background()

	inherited, err := m.Create(ctx, CreateRequest{Name: "inherits-operator-default"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inherited.AutoMode != config.AutoModeOn {
		t.Errorf("AutoMode = %q, want %q (the operator default)", inherited.AutoMode, config.AutoModeOn)
	}
	spec, err := m.agentSpec(inherited, resolvedBackend{kind: inherited.Backend}, autoModeArgs(inherited)...)
	if err != nil {
		t.Fatalf("agentSpec: %v", err)
	}
	if len(spec.Cmd) != 3 || spec.Cmd[1] != "--permission-mode" || spec.Cmd[2] != "auto" {
		t.Errorf("Cmd = %v, want [%q --permission-mode auto]", spec.Cmd, tmuxBootPath)
	}

	overridden, err := m.Create(ctx, CreateRequest{Name: "explicit-off", AutoMode: config.AutoModeOff})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if overridden.AutoMode != config.AutoModeOff {
		t.Errorf("AutoMode = %q, want %q (the per-agent override)", overridden.AutoMode, config.AutoModeOff)
	}

	before, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := m.Create(ctx, CreateRequest{Name: "bad", AutoMode: "sometimes"}); !IsInvalidAutoMode(err) {
		t.Errorf("Create with auto_mode=%q: err = %v, want IsInvalidAutoMode", "sometimes", err)
	}
	after, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("an invalid auto_mode consumed a slot: %d records before, %d after", len(before), len(after))
	}
}

// newFakeDindContainer creates and starts a bare fake container standing in
// for a dind sidecar, so ExecCreate has a real container ID to resolve --
// unlike a made-up string, which ExecCreate correctly rejects as not-found
// (and runExec then never reaches ExecAttach/ExecInspect at all).
func newFakeDindContainer(t *testing.T, f *dockerclienttest.Fake) string {
	t.Helper()
	ctx := context.Background()
	id, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "test-dind", Image: dindImage})
	if err != nil {
		t.Fatalf("seeding a fake dind container: %v", err)
	}
	if err := f.ContainerStart(ctx, id); err != nil {
		t.Fatalf("starting the fake dind container: %v", err)
	}
	return id
}

// TestVerifyDependaproxyReachable exercises the standalone reachability check
// directly (not through the full Create flow, which always succeeds it on
// the first try since the fake's exec commands default to exit 0) -- see the
// function's own doc comment in create.go for why it exists and why it never
// fails its caller.
func TestVerifyDependaproxyReachable(t *testing.T) {
	t.Run("skips the check entirely when no dependaproxy address is recorded", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		a := store.Agent{ID: "agt_test", DindContainerID: "dind123"}
		before := len(f.Calls())
		m.verifyDependaproxyReachable(context.Background(), a)
		if got := len(f.Calls()) - before; got != 0 {
			t.Errorf("Calls() made = %d, want 0 for a record with no DependaproxyDinernetIP", got)
		}
	})

	t.Run("returns immediately on a first-try success, against the dind container", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		dindID := newFakeDindContainer(t, f)
		a := store.Agent{ID: "agt_test", DindContainerID: dindID, DependaproxyDinernetIP: netip.MustParseAddr("10.0.5.9")}
		before := len(f.Calls())
		start := time.Now()
		m.verifyDependaproxyReachable(context.Background(), a)
		if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
			t.Errorf("took %s, want it to return almost immediately on a first-try success", elapsed)
		}
		checkExecTriplet(t, f.Calls()[before:], a.DindContainerID, "verifyDependaproxyReachable (first-try success)")
	})

	t.Run("retries for the configured budget, then degrades without erroring when never reachable", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		dindID := newFakeDindContainer(t, f)
		a := store.Agent{ID: "agt_test", DindContainerID: dindID, DependaproxyDinernetIP: netip.MustParseAddr("10.0.5.9")}
		f.ExecExit["nc -z -w2 -n 10.0.5.9 8080"] = 1 // every attempt fails
		before := len(f.Calls())
		start := time.Now()
		m.verifyDependaproxyReachable(context.Background(), a) // must not panic/error -- best-effort only
		if elapsed := time.Since(start); elapsed < m.opts.DependaproxyReachableTimeout {
			t.Errorf("returned after %s, want it to keep retrying for the full %s budget", elapsed, m.opts.DependaproxyReachableTimeout)
		}
		calls := f.Calls()[before:]
		if len(calls) < 6 {
			t.Errorf("Calls() = %v, want at least two retried attempts (3 calls each)", calls)
		}
		for len(calls) > 0 {
			calls = checkExecTriplet(t, calls, a.DindContainerID, "verifyDependaproxyReachable (retry)")
		}
	})
}

// TestSyncDependaproxyDinernetIP exercises syncDependaproxyDinernetIP's
// decision table directly against a real agent record produced by Create, so
// each sub-test starts from a genuinely-attached dinernet rather than a
// hand-built fixture.
func TestSyncDependaproxyDinernetIP(t *testing.T) {
	t.Run("already correct: no-op, exactly one ContainerInspect call", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)
		before := f.Calls()

		got := m.syncDependaproxyDinernetIP(ctx, &a)
		if got {
			t.Error("syncDependaproxyDinernetIP = true, want false (nothing had drifted)")
		}
		newCalls := f.Calls()[len(before):]
		if len(newCalls) != 1 || newCalls[0] != (dockerclienttest.Call{Op: dockerclienttest.OpContainerInspect, Target: m.cfg.DependaproxyContainer}) {
			t.Errorf("Calls() made = %v, want exactly one ContainerInspect of the dependaproxy container", newCalls)
		}
	})

	t.Run("not attached: reconnects and restamps", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)
		oldAddr := a.DependaproxyDinernetIP

		if err := f.NetworkDisconnect(ctx, a.DinernetName, m.cfg.DependaproxyContainer); err != nil {
			t.Fatalf("simulating the shared dependaproxy container losing its attachment: %v", err)
		}
		before := f.Calls()

		got := m.syncDependaproxyDinernetIP(ctx, &a)
		if !got {
			t.Fatal("syncDependaproxyDinernetIP = false, want true (the attachment was gone)")
		}
		if !a.DependaproxyDinernetIP.IsValid() || a.DependaproxyDinernetIP == oldAddr {
			t.Errorf("DependaproxyDinernetIP = %v, want a freshly assigned address (old was %v)", a.DependaproxyDinernetIP, oldAddr)
		}
		newCalls := f.Calls()[len(before):]
		wantTarget := a.DinernetName + "/" + m.cfg.DependaproxyContainer
		if !hasCall(newCalls, dockerclienttest.OpNetworkConnect, wantTarget) {
			t.Errorf("Calls() = %v, want a NetworkConnect(%q)", newCalls, wantTarget)
		}

		// The store record itself must carry the new address too.
		stored, err := m.Get(ctx, a.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.DependaproxyDinernetIP != a.DependaproxyDinernetIP {
			t.Errorf("stored DependaproxyDinernetIP = %v, want %v", stored.DependaproxyDinernetIP, a.DependaproxyDinernetIP)
		}
	})

	t.Run("attached but stale: restamps without reconnecting", func(t *testing.T) {
		m, f, st := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)
		realAddr := a.DependaproxyDinernetIP

		corrupted, err := st.Update(ctx, a.ID, func(ag *store.Agent) error {
			ag.DependaproxyDinernetIP = netip.MustParseAddr("10.9.9.9")
			return nil
		})
		if err != nil {
			t.Fatalf("corrupting the recorded address: %v", err)
		}
		before := f.Calls()

		got := m.syncDependaproxyDinernetIP(ctx, &corrupted)
		if !got {
			t.Fatal("syncDependaproxyDinernetIP = false, want true (the recorded address disagreed with reality)")
		}
		if corrupted.DependaproxyDinernetIP != realAddr {
			t.Errorf("DependaproxyDinernetIP = %v, want it restored to the real address %v", corrupted.DependaproxyDinernetIP, realAddr)
		}
		newCalls := f.Calls()[len(before):]
		if hasOp(newCalls, dockerclienttest.OpNetworkConnect) {
			t.Errorf("Calls() = %v, want no NetworkConnect (already attached, just re-stamp)", newCalls)
		}
	})

	t.Run("dependaproxy gone: left untouched", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)
		oldAddr := a.DependaproxyDinernetIP

		if err := f.ContainerRemove(ctx, m.cfg.DependaproxyContainer); err != nil {
			t.Fatalf("removing the shared dependaproxy container: %v", err)
		}
		before := f.Calls()

		got := m.syncDependaproxyDinernetIP(ctx, &a)
		if got {
			t.Error("syncDependaproxyDinernetIP = true, want false (dependaproxy is gone, nothing to sync against)")
		}
		if a.DependaproxyDinernetIP != oldAddr {
			t.Errorf("DependaproxyDinernetIP = %v, want it left at %v", a.DependaproxyDinernetIP, oldAddr)
		}
		newCalls := f.Calls()[len(before):]
		if hasOp(newCalls, dockerclienttest.OpNetworkConnect) {
			t.Errorf("Calls() = %v, want no NetworkConnect attempt", newCalls)
		}
	})

	t.Run("dependaproxy present but stopped and detached: no connect attempt", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)

		if err := f.NetworkDisconnect(ctx, a.DinernetName, m.cfg.DependaproxyContainer); err != nil {
			t.Fatalf("disconnecting: %v", err)
		}
		if err := f.ContainerStop(ctx, m.cfg.DependaproxyContainer, 0); err != nil {
			t.Fatalf("stopping the shared dependaproxy container: %v", err)
		}
		before := f.Calls()

		got := m.syncDependaproxyDinernetIP(ctx, &a)
		if got {
			t.Error("syncDependaproxyDinernetIP = true, want false (dependaproxy is stopped, cannot be given an address)")
		}
		newCalls := f.Calls()[len(before):]
		if hasOp(newCalls, dockerclienttest.OpNetworkConnect) {
			t.Errorf("Calls() = %v, want no NetworkConnect attempt against a stopped container", newCalls)
		}
	})

	t.Run("NetworkConnect fails: best-effort no-op", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)
		oldAddr := a.DependaproxyDinernetIP

		if err := f.NetworkDisconnect(ctx, a.DinernetName, m.cfg.DependaproxyContainer); err != nil {
			t.Fatalf("disconnecting: %v", err)
		}
		f.FailOnce(dockerclienttest.OpNetworkConnect, errors.New("boom: network connect"))

		got := m.syncDependaproxyDinernetIP(ctx, &a)
		if got {
			t.Error("syncDependaproxyDinernetIP = true, want false (the reconnect attempt failed)")
		}
		if a.DependaproxyDinernetIP != oldAddr {
			t.Errorf("DependaproxyDinernetIP = %v, want it left at %v after a failed reconnect", a.DependaproxyDinernetIP, oldAddr)
		}
	})

	t.Run("no dinernet recorded: zero docker calls", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		a := store.Agent{ID: "agt_bare"}
		before := len(f.Calls())

		got := m.syncDependaproxyDinernetIP(context.Background(), &a)
		if got {
			t.Error("syncDependaproxyDinernetIP = true, want false")
		}
		if after := len(f.Calls()); after != before {
			t.Errorf("Calls() made = %d, want 0 for a record with no DinernetName", after-before)
		}
	})
}

// TestRefreshDependaproxyIPFile exercises refreshDependaproxyIPFile directly.
func TestRefreshDependaproxyIPFile(t *testing.T) {
	t.Run("writes the address into the container as an exec argument, not interpolated into the script", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)
		before := len(f.Calls())

		m.refreshDependaproxyIPFile(ctx, a)

		newCalls := f.Calls()[before:]
		checkExecTriplet(t, newCalls, a.ContainerID, "refreshDependaproxyIPFile")

		specs := f.ExecSpecs()
		if len(specs) == 0 {
			t.Fatal("no ExecSpec recorded")
		}
		spec := specs[len(specs)-1]
		if len(spec.Cmd) != 4 || spec.Cmd[0] != "sh" || spec.Cmd[1] != "-c" {
			t.Fatalf("ExecSpec.Cmd = %v, want [sh -c <script> <addr>]", spec.Cmd)
		}
		addr := a.DependaproxyDinernetIP.String()
		if spec.Cmd[3] != addr {
			t.Errorf("ExecSpec.Cmd[3] = %q, want the address %q passed as its own argument", spec.Cmd[3], addr)
		}
		if strings.Contains(spec.Cmd[2], addr) {
			t.Errorf("ExecSpec.Cmd[2] (the script) = %q, want the address kept OUT of the script text and passed as $0 instead", spec.Cmd[2])
		}
		if !strings.Contains(spec.Cmd[2], dependaproxyIPPath) {
			t.Errorf("ExecSpec.Cmd[2] = %q, want it to mention %q", spec.Cmd[2], dependaproxyIPPath)
		}
	})

	t.Run("skips entirely when no address is recorded", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		a := store.Agent{ID: "agt_test", ContainerID: "whatever"}
		before := len(f.Calls())

		m.refreshDependaproxyIPFile(context.Background(), a)

		if after := len(f.Calls()); after != before {
			t.Errorf("Calls() made = %d, want 0 when DependaproxyDinernetIP is not recorded", after-before)
		}
	})

	t.Run("a non-zero exit degrades quietly with no retry", func(t *testing.T) {
		m, f, _ := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)

		script := "printf '%s\\n' \"$0\" > " + dependaproxyIPPath + ".tmp && mv " + dependaproxyIPPath + ".tmp " + dependaproxyIPPath
		key := strings.Join([]string{"sh", "-c", script, a.DependaproxyDinernetIP.String()}, " ")
		f.ExecExit[key] = 1
		before := len(f.Calls())

		m.refreshDependaproxyIPFile(ctx, a) // must not panic; best-effort only

		newCalls := f.Calls()[before:]
		execCreates := 0
		for _, c := range newCalls {
			if c.Op == dockerclienttest.OpExecCreate {
				execCreates++
			}
		}
		if execCreates != 1 {
			t.Errorf("ExecCreate calls = %d, want exactly 1 (no retry on a non-zero exit)", execCreates)
		}
	})
}
