package operator

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/psenna/ai-sandbox/operator/internal/config"
)

func baseConfig() config.Config {
	return config.Config{
		SlotCapacity:         4,
		ClusterID:            "default",
		DefaultSandboxClass:  "default",
		MetricsAddr:          "0",
		HealthProbeAddr:      "0",
		EnableLeaderElection: false,
	}
}

// TestNew_ConstructsWithoutCluster verifies the manager can be built without
// dialing a real API server: controller-runtime's REST mapper is lazy, so
// construction alone must not require connectivity.
func TestNew_ConstructsWithoutCluster(t *testing.T) {
	restCfg := &rest.Config{Host: "https://127.0.0.1:6443"}

	mgr, err := New(restCfg, baseConfig())
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	if mgr == nil {
		t.Fatal("New() returned nil manager with nil error")
	}
}

// TestManager_ServesProbesAndShutsDownCleanly proves the core "manager
// constructs and shuts down without a cluster" smoke test at the unit
// level: it starts the manager against a fake REST host, waits for
// /healthz and /readyz to respond, cancels the context, and asserts Start
// returns promptly with no error.
func TestManager_ServesProbesAndShutsDownCleanly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a free port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing reserved listener: %v", err)
	}

	cfg := baseConfig()
	cfg.HealthProbeAddr = addr

	restCfg := &rest.Config{Host: "https://127.0.0.1:6443"}
	mgr, err := New(restCfg, cfg)
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErr := make(chan error, 1)
	go func() {
		startErr <- mgr.Start(ctx)
	}()

	baseURL := fmt.Sprintf("http://%s", addr)
	waitForOK(t, baseURL+"/healthz", 30*time.Second)

	resp, err := http.Get(baseURL + "/readyz") //nolint:gosec // test-only, address is a local ephemeral port we picked above
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want 200", resp.StatusCode)
	}

	cancel()

	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("mgr.Start returned error after shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("mgr.Start did not return within 30s of context cancellation")
	}
}

// TestNew_WatchNamespaceScopesCache verifies a non-empty WatchNamespace is
// accepted and does not prevent construction.
func TestNew_WatchNamespaceScopesCache(t *testing.T) {
	cfg := baseConfig()
	cfg.WatchNamespace = "some-ns"

	restCfg := &rest.Config{Host: "https://127.0.0.1:6443"}
	mgr, err := New(restCfg, cfg)
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	if mgr == nil {
		t.Fatal("New() returned nil manager with nil error")
	}
}

func waitForOK(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:gosec // test-only, url is built from a local ephemeral address above
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not become ready within %s", url, timeout)
}
