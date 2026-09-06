package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

func storeMountCount(spec dockerclient.ContainerSpec) (total, storeMounts int) {
	for _, m := range spec.Mounts {
		total++
		if m.Target == agentStoreMount {
			storeMounts++
		}
	}
	return total, storeMounts
}

func TestFilestore_DisabledByDefault(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	a := store.Agent{ID: "agt_disabled"}

	total, storeMounts := storeMountCount(m.agentSpec(a, resolvedBackend{kind: "ollama"}))
	if total != 2 || storeMounts != 0 {
		t.Errorf("mounts = %d (store %d), want exactly 2 and none targeting %q", total, storeMounts, agentStoreMount)
	}
	if _, ok := m.agentEnv(a, resolvedBackend{kind: "ollama"})["AGENT_STORE_DIR"]; ok {
		t.Error("AGENT_STORE_DIR set with the file store disabled")
	}
	if err := m.PurgeAgentFiles(context.Background(), "agt_disabled"); err != nil {
		t.Errorf("PurgeAgentFiles (disabled) = %v, want nil", err)
	}
}

func TestFilestore_EnabledSpecAndEnv(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, _, _ := newTestManagerCfg(t, cfg)
	a := store.Agent{ID: "agt_enabled"}

	spec := m.agentSpec(a, resolvedBackend{kind: "ollama"})
	total, storeMounts := storeMountCount(spec)
	if total != 3 || storeMounts != 1 {
		t.Fatalf("mounts = %d (store %d), want 3 with one file-store mount", total, storeMounts)
	}
	last := spec.Mounts[2]
	if last.Type != dockerclient.MountTypeVolume || last.Source != cfg.FilestoreVolume ||
		last.Target != "/workspace/store" || last.Subpath != "agents/agt_enabled" {
		t.Errorf("file-store mount = %+v", last)
	}
	if got := m.agentEnv(a, resolvedBackend{kind: "ollama"})["AGENT_STORE_DIR"]; got != "/workspace/store" {
		t.Errorf("AGENT_STORE_DIR = %q, want /workspace/store", got)
	}
}

func TestFilestore_CreateMakesAgentDir(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, _, _ := newTestManagerCfg(t, cfg)

	a, err := m.Create(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir := filepath.Join(cfg.FilestoreDir, "agents", a.ID)
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %q: %v", dir, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o777 {
		t.Errorf("perm = %o, want 0777", perm)
	}
}

func TestFilestore_FilesSurviveDelete(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, _, _ := newTestManagerCfg(t, cfg)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	dir := filepath.Join(cfg.FilestoreDir, "agents", a.ID)
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("agent file-store dir gone after Delete: %v (it must persist)", err)
	}
}

func TestFilestore_Purge(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, _, _ := newTestManagerCfg(t, cfg)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir := filepath.Join(cfg.FilestoreDir, "agents", a.ID)
	if err := os.WriteFile(filepath.Join(dir, "artifact.txt"), []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := m.PurgeAgentFiles(ctx, a.ID); err != nil {
		t.Fatalf("PurgeAgentFiles: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir still present after purge: %v", err)
	}
	if err := m.PurgeAgentFiles(ctx, a.ID); err != nil {
		t.Errorf("second PurgeAgentFiles = %v, want nil (idempotent)", err)
	}
}

func TestFilestore_RollbackKeepsAgentDir(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	m, f, _ := newTestManagerCfg(t, cfg)

	// The first ContainerCreate is the DinD sidecar, created after
	// ensureAgentFiles has already made agents/<id>/.
	f.FailOnce(dockerclienttest.OpContainerCreate, errors.New("boom: dind create"))

	if _, err := m.Create(context.Background(), CreateRequest{}); err == nil {
		t.Fatal("Create = nil, want the injected failure")
	}

	entries, err := os.ReadDir(filepath.Join(cfg.FilestoreDir, "agents"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("agents/ has %d entries after rollback, want the orphan to survive", len(entries))
	}
}

func TestFilestore_UnusableConfigDegrades(t *testing.T) {
	cfg := testConfigWithFilestore(t, 5)
	// Point FilestoreDir at an existing regular file: filestore.New fails,
	// and NewManager must build the manager anyway with m.files == nil.
	p := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.FilestoreDir = p

	m, _, _ := newTestManagerCfg(t, cfg)
	if m.files != nil {
		t.Fatal("m.files is non-nil despite an unusable FilestoreDir")
	}
	total, storeMounts := storeMountCount(m.agentSpec(store.Agent{ID: "agt_x"}, resolvedBackend{kind: "ollama"}))
	if total != 2 || storeMounts != 0 {
		t.Errorf("mounts = %d (store %d), want graceful degradation to 2", total, storeMounts)
	}
}
