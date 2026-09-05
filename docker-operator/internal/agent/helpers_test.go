package agent

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

const testAgentImage = "test-agent-image:dev"

// testConfig returns a Config that exercises every field internal/agent
// reads, with values distinct enough from any real default that a test
// mixing up a name and a value fails loudly.
func testConfig(maxAgents int) config.Config {
	return config.Config{
		MaxAgents:              maxAgents,
		AgentImage:             testAgentImage,
		ProxynetName:           "test-proxynet",
		DbnetName:              "test-dbnet",
		GithubRepo:             "psenna/test.git",
		AgentToken:             config.Secret("test-agent-token"),
		GitProxyURL:            "http://git-proxy:8080",
		GitProxyBrokerURL:      "http://git-proxy:8090",
		DependaproxyURL:        "http://dependaproxy:8080/npm",
		DependaproxyPyPIURL:    "http://dependaproxy:8080/pypi",
		DependaproxyGoproxyURL: "http://dependaproxy:8080/goproxy",
		DockerRuntime:          "sysbox-runc",
		DependaproxyContainer:  "test-dependaproxy",
		// The default backend for a create request that names none. Every
		// backend-specific test overrides the request's Backend explicitly.
		DefaultBackend:     config.BackendOllama,
		OllamaURL:          "http://ollama:11434",
		AnthropicAuthToken: config.Secret("test-anthropic-auth"),
		AgentModel:         "test-opus-model",
		AgentFastModel:     "test-fast-model",
	}
}

// testOptions shrinks every timeout so the timeout-path tests run fast, while
// keeping PollInterval short enough that even a 100ms timeout gets several
// polls.
func testOptions() Options {
	return Options{
		DindHealthTimeout: 500 * time.Millisecond,
		TmuxReadyTimeout:  500 * time.Millisecond,
		ExecTimeout:       500 * time.Millisecond,
		PollInterval:      10 * time.Millisecond,
		StopTimeout:       200 * time.Millisecond,
		TeardownTimeout:   5 * time.Second,
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestStore opens a fresh BoltDB-backed store under t.TempDir(), closed
// automatically at test cleanup.
func newTestStore(t *testing.T, maxAgents int) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), maxAgents)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newDependaproxy creates and starts a fake container standing in for the
// shared dependaproxy container every agent's dinernet connects to -- without
// it running, connectDependaproxy has nothing to assign an address to.
func newDependaproxy(t *testing.T, f *dockerclienttest.Fake, name string) {
	t.Helper()
	ctx := context.Background()
	id, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: name, Image: "dependaproxy:test"})
	if err != nil {
		t.Fatalf("seeding the dependaproxy container: %v", err)
	}
	if err := f.ContainerStart(ctx, id); err != nil {
		t.Fatalf("starting the dependaproxy container: %v", err)
	}
}

// newTestManager wires a Manager against a fresh Fake docker client and a
// fresh BoltDB store, with the shared dependaproxy container already running
// and both images pre-seeded as present. Tests that specifically want to
// exercise image-pull or dependaproxy-absent behaviour build their own Fake
// instead of using this helper.
func newTestManager(t *testing.T, maxAgents int) (*Manager, *dockerclienttest.Fake, *store.Store) {
	t.Helper()
	f := dockerclienttest.New()
	f.AutoHealthy = true
	cfg := testConfig(maxAgents)
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(dindImage)
	f.AddImage(cfg.AgentImage)
	st := newTestStore(t, maxAgents)
	m := NewManager(f, st, cfg, testLogger(), testOptions())
	return m, f, st
}

// resourceCounts snapshots how many volumes/networks/containers the fake
// currently holds, so a rollback test can assert a failed create leaves the
// daemon exactly as it found it (accounting for the pre-existing shared
// dependaproxy container, which Manager never creates or removes).
type resourceCounts struct{ volumes, networks, containers int }

func snapshotCounts(f *dockerclienttest.Fake) resourceCounts {
	return resourceCounts{
		volumes:    len(f.Volumes()),
		networks:   len(f.Networks()),
		containers: len(f.Containers()),
	}
}
