package operator

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/config"
)

func baseConfig() config.Config {
	return config.Config{
		SlotCapacity:        4,
		ClusterID:           "default",
		DefaultSandboxClass: "default",
		MetricsAddr:         "0",
		HealthProbeAddr:     "0",
		// SchedulerInterval and WarmCacheGCInterval are left at their zero
		// values here: SetupControllers only registers the runnables, and
		// their Start is never invoked in these no-cluster tests.
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

// TestScheme_RecognizesSandboxAPITypes verifies the scheme built by Scheme()
// knows about both v1alpha1 API kinds, so the manager can decode/encode
// them once controllers are registered in issue #18.
func TestScheme_RecognizesSandboxAPITypes(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatalf("Scheme() returned error: %v", err)
	}

	for _, kind := range []string{"SandboxClass", "SandboxEnvironment"} {
		gvk := sandboxv1alpha1.GroupVersion.WithKind(kind)
		if !scheme.Recognizes(gvk) {
			t.Errorf("scheme does not recognize %s", gvk)
		}
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
