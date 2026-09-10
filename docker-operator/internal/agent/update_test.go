package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// createRunningAgent runs the full create flow against newTestManager and
// returns the resulting StatusRunning record.
func createRunningAgent(t *testing.T, m *Manager) store.Agent {
	t.Helper()
	a, err := m.Create(context.Background(), CreateRequest{Name: "before"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Status != store.StatusRunning {
		t.Fatalf("seeded agent status = %q, want running", a.Status)
	}
	return a
}

// callsAfter returns the Fake calls recorded after index n.
func callsAfter(f *dockerclienttest.Fake, n int) []dockerclienttest.Call {
	all := f.Calls()
	if n >= len(all) {
		return nil
	}
	return all[n:]
}

func hasCall(calls []dockerclienttest.Call, op dockerclienttest.Op, target string) bool {
	for _, c := range calls {
		if c.Op == op && c.Target == target {
			return true
		}
	}
	return false
}

func hasOp(calls []dockerclienttest.Call, op dockerclienttest.Op) bool {
	for _, c := range calls {
		if c.Op == op {
			return true
		}
	}
	return false
}

func volumeNames(f *dockerclienttest.Fake) map[string]bool {
	out := map[string]bool{}
	for _, v := range f.Volumes() {
		out[v.Name] = true
	}
	return out
}

func TestUpdate_RecreatesOnlyAgentContainer(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)
	mark := len(f.Calls())

	updated, err := m.Update(ctx, a.ID, UpdateRequest{ImageTag: "20260101-000000"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != store.StatusRunning {
		t.Fatalf("updated status = %q, want running", updated.Status)
	}

	calls := callsAfter(f, mark)
	newRef := RepoWithoutTag(testAgentImage) + ":20260101-000000"
	if !hasCall(calls, dockerclienttest.OpImagePull, newRef) {
		t.Errorf("no ImagePull for the new ref %q; calls=%v", newRef, calls)
	}
	if !hasCall(calls, dockerclienttest.OpContainerStop, a.ContainerID) {
		t.Errorf("no ContainerStop for the agent container %q; calls=%v", a.ContainerID, calls)
	}
	if !hasCall(calls, dockerclienttest.OpContainerRemove, a.ContainerID) {
		t.Errorf("no ContainerRemove for the agent container %q; calls=%v", a.ContainerID, calls)
	}
	if !hasCall(calls, dockerclienttest.OpContainerCreate, a.ContainerName) {
		t.Errorf("no ContainerCreate for the agent container name %q; calls=%v", a.ContainerName, calls)
	}
	// Nothing else may be touched.
	for _, op := range []dockerclienttest.Op{
		dockerclienttest.OpNetworkRemove, dockerclienttest.OpNetworkDisconnect,
		dockerclienttest.OpVolumeRemove, dockerclienttest.OpNetworkCreate,
	} {
		if hasOp(calls, op) {
			t.Errorf("Update performed %s; it must recreate the agent container only. calls=%v", op, calls)
		}
	}
	// The DinD sidecar must not be stopped/removed.
	if hasCall(calls, dockerclienttest.OpContainerStop, a.DindContainerID) ||
		hasCall(calls, dockerclienttest.OpContainerRemove, a.DindContainerID) {
		t.Errorf("Update touched the DinD sidecar %q; calls=%v", a.DindContainerID, calls)
	}

	// Volumes still present.
	vols := volumeNames(f)
	for _, v := range []string{a.WorkspaceVolume, a.ClaudeConfigVolume, a.DindCacheVolume} {
		if !vols[v] {
			t.Errorf("volume %q is gone after Update", v)
		}
	}
	_ = st
}

func TestUpdate_RecreatedCmdHasContinue(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	a := createRunningAgent(t, m)
	spec := m.agentSpec(a, resolvedBackend{kind: config.BackendOllama}, "--continue")
	if len(spec.Cmd) != 2 || spec.Cmd[0] != tmuxBootPath || spec.Cmd[1] != "--continue" {
		t.Fatalf("recreated Cmd = %v, want [%q --continue]", spec.Cmd, tmuxBootPath)
	}
}

func TestUpdate_KeepsIDAndVolumeNames(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)

	updated, err := m.Update(ctx, a.ID, UpdateRequest{ImageTag: "20260101-000000"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.ID != a.ID {
		t.Errorf("ID = %q, want %q (immutable)", updated.ID, a.ID)
	}
	if updated.WorkspaceVolume != a.WorkspaceVolume ||
		updated.ClaudeConfigVolume != a.ClaudeConfigVolume ||
		updated.DindCacheVolume != a.DindCacheVolume {
		t.Errorf("volume names changed: %+v vs %+v", updated, a)
	}
	if updated.Image != RepoWithoutTag(testAgentImage)+":20260101-000000" {
		t.Errorf("Image = %q, want the new ref", updated.Image)
	}
}

func TestUpdate_BackendOllamaToAnthropic(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()
	if err := st.SetAnthropicAuth(ctx, store.AnthropicKindOAuth, "oat-live"); err != nil {
		t.Fatalf("SetAnthropicAuth: %v", err)
	}
	a := createRunningAgent(t, m) // ollama by default

	updated, err := m.Update(ctx, a.ID, UpdateRequest{
		CreateRequest: CreateRequest{Backend: config.BackendAnthropic},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Backend != config.BackendAnthropic {
		t.Errorf("Backend = %q, want anthropic", updated.Backend)
	}
	if updated.OllamaURL != "" || updated.Model != "" || updated.FastModel != "" {
		t.Errorf("ollama fields not cleared: %+v", updated)
	}

	rb, err := m.resolveBackend(ctx, CreateRequest{Backend: config.BackendAnthropic})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	env := m.agentEnv(updated, rb)
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != "oat-live" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q, want the seeded credential", env["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	if _, ok := env["ANTHROPIC_BASE_URL"]; ok {
		t.Errorf("ANTHROPIC_BASE_URL is set for an anthropic agent: %q", env["ANTHROPIC_BASE_URL"])
	}
}

func TestUpdate_ImagePullFailsBeforeRemoval_AgentUntouched(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)
	mark := len(f.Calls())

	f.FailOnce(dockerclienttest.OpImagePull, errors.New("registry unreachable"))

	if _, err := m.Update(ctx, a.ID, UpdateRequest{ImageTag: "20260101-000000"}); err == nil {
		t.Fatal("Update succeeded, want the image-pull failure")
	}

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusError {
		t.Errorf("Status = %q, want error (it was already marked updating before the pull)", got.Status)
	}
	if !strings.Contains(got.ErrorMessage, "update failed") {
		t.Errorf("ErrorMessage = %q, want it to mention the failed update", got.ErrorMessage)
	}
	calls := callsAfter(f, mark)
	if hasOp(calls, dockerclienttest.OpContainerRemove) {
		t.Errorf("the agent container was removed despite the pull failing first; calls=%v", calls)
	}
	for _, v := range []string{a.WorkspaceVolume, a.ClaudeConfigVolume, a.DindCacheVolume} {
		if !volumeNames(f)[v] {
			t.Errorf("volume %q is gone", v)
		}
	}
}

func TestUpdate_ContainerCreateFails_MarksErrorKeepsVolumes(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)

	f.FailOnce(dockerclienttest.OpContainerCreate, errors.New("no space left on device"))

	if _, err := m.Update(ctx, a.ID, UpdateRequest{ImageTag: "20260101-000000"}); err == nil {
		t.Fatal("Update succeeded, want the container-create failure")
	}

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v (the record must survive)", err)
	}
	if got.Status != store.StatusError {
		t.Errorf("Status = %q, want error", got.Status)
	}
	if got.ContainerID != "" {
		t.Errorf("ContainerID = %q, want empty after a failed update", got.ContainerID)
	}
	for _, v := range []string{a.WorkspaceVolume, a.ClaudeConfigVolume, a.DindCacheVolume} {
		if !volumeNames(f)[v] {
			t.Errorf("volume %q is gone", v)
		}
	}
}

func TestUpdate_NotFound(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	_, err := m.Update(context.Background(), "agt_missing", UpdateRequest{})
	if !store.IsNotFound(err) {
		t.Fatalf("err = %v, want store.IsNotFound", err)
	}
}

func TestUpdate_RejectsCreatingOrDeleting(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()
	a, err := st.Create(ctx, store.CreateSpec{ID: "agt_creating"})
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	if _, err := m.Update(ctx, a.ID, UpdateRequest{}); !IsNotUpdatable(err) {
		t.Fatalf("err = %v, want IsNotUpdatable", err)
	}
}

func TestUpdate_InvalidImageTag(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)

	_, err := m.Update(ctx, a.ID, UpdateRequest{ImageTag: "not a tag!"})
	if !IsInvalidImageTag(err) {
		t.Fatalf("err = %v, want IsInvalidImageTag", err)
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("Status = %q, want running (validation happens before any mutation)", got.Status)
	}
}

func TestUpdate_TransientStatusMakesDieEventNoop(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)

	if _, err := st.Update(ctx, a.ID, func(ag *store.Agent) error {
		ag.Status = store.StatusUpdating
		return nil
	}); err != nil {
		t.Fatalf("marking updating: %v", err)
	}

	if err := m.MarkUnexpectedExit(ctx, a.ID, a.ContainerID, store.StatusError, "container die unexpectedly"); err != nil {
		t.Fatalf("MarkUnexpectedExit: %v", err)
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusUpdating {
		t.Errorf("Status = %q, want updating (a die event during an update must be a no-op)", got.Status)
	}
}
