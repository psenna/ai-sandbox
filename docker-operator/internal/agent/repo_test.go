package agent

import (
	"context"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// managerWithRepo wires a Manager exactly like newTestManager but with a
// caller-chosen operator-default repo (including "" for the bare-agent case).
func managerWithRepo(t *testing.T, repo string) (*Manager, *dockerclienttest.Fake, *store.Store) {
	t.Helper()
	f := dockerclienttest.New()
	f.AutoHealthy = true
	cfg := testConfig(5)
	cfg.GithubRepo = repo
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(dindImage)
	f.AddImage(cfg.AgentImage)
	st := newTestStore(t, 5)
	return NewManager(f, st, cfg, testLogger(), testOptions()), f, st
}

func TestAgentEnv_Repo(t *testing.T) {
	m, _, _ := newTestManager(t, 5)

	t.Run("GITHUB_REPO comes from the agent record, not the config", func(t *testing.T) {
		a := store.Agent{ID: "a", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n", Repo: "acme/widget.git"}
		env := m.agentEnv(a, resolvedBackend{kind: config.BackendOllama, model: "o", fastModel: "f"})
		wantEq(t, env, "GITHUB_REPO", "acme/widget.git")
	})

	t.Run("a bare agent gets an empty GITHUB_REPO", func(t *testing.T) {
		a := store.Agent{ID: "a", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n"}
		env := m.agentEnv(a, resolvedBackend{kind: config.BackendOllama, model: "o", fastModel: "f"})
		wantEq(t, env, "GITHUB_REPO", "")
	})
}

func TestCreate_ResolvesRepo(t *testing.T) {
	ctx := context.Background()

	t.Run("no request repo falls back to the operator default", func(t *testing.T) {
		m, _, _ := managerWithRepo(t, "psenna/ai-sandbox.git")
		got, err := m.Create(ctx, CreateRequest{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.Repo != "psenna/ai-sandbox.git" {
			t.Errorf("record.Repo = %q, want the operator default", got.Repo)
		}
	})

	t.Run("a per-agent repo overrides the operator default", func(t *testing.T) {
		m, _, _ := managerWithRepo(t, "psenna/ai-sandbox.git")
		got, err := m.Create(ctx, CreateRequest{Repo: "other/project"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.Repo != "other/project" {
			t.Errorf("record.Repo = %q, want the per-agent value", got.Repo)
		}
	})

	t.Run("no default and no request repo: a bare agent", func(t *testing.T) {
		m, _, _ := managerWithRepo(t, "")
		got, err := m.Create(ctx, CreateRequest{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.Repo != "" {
			t.Errorf("record.Repo = %q, want empty", got.Repo)
		}
	})

	t.Run("a malformed repo is rejected and consumes nothing", func(t *testing.T) {
		m, f, st := managerWithRepo(t, "")
		before := snapshotCounts(f)
		_, err := m.Create(ctx, CreateRequest{Repo: "https://github.com/psenna/ai-sandbox"})
		if !IsInvalidRepo(err) {
			t.Fatalf("Create err = %v, want IsInvalidRepo", err)
		}
		if after := snapshotCounts(f); after != before {
			t.Errorf("docker resources = %+v, want unchanged %+v", after, before)
		}
		if agents, _ := st.List(ctx); len(agents) != 0 {
			t.Errorf("store records = %v, want none (no slot consumed)", agents)
		}
	})

	// A configured operator default is validated at config load, but guard
	// the create path too: a bad default must not silently ship to an agent.
	t.Run("a malformed operator default is rejected", func(t *testing.T) {
		m, _, _ := managerWithRepo(t, "not-a-repo")
		if _, err := m.Create(ctx, CreateRequest{}); !IsInvalidRepo(err) {
			t.Fatalf("Create err = %v, want IsInvalidRepo", err)
		}
	})
}
