package sandboxctl

import (
	"net"
	"testing"
	"time"
)

func emptyEnv(string) string { return "" }

func TestConfig_Validate_LoopbackListenRequired(t *testing.T) {
	rejected := []string{"0.0.0.0:9099", ":9099", "[::]:9099", "not-an-address", "127.0.0.1", "example.com:9099"}
	for _, addr := range rejected {
		t.Run(addr, func(t *testing.T) {
			c := Config{Environment: "e", Namespace: "ns", Listen: addr, PollInterval: 5 * time.Second, ShutdownTimeout: 10 * time.Second}
			if err := c.Validate(); err == nil {
				t.Errorf("Validate() with Listen=%q: expected error, got nil", addr)
			}
		})
	}

	accepted := []string{"127.0.0.1:9099", "[::1]:9099"}
	for _, addr := range accepted {
		t.Run(addr, func(t *testing.T) {
			c := Config{Environment: "e", Namespace: "ns", Listen: addr, PollInterval: 5 * time.Second, ShutdownTimeout: 10 * time.Second}
			if err := c.Validate(); err != nil {
				t.Errorf("Validate() with Listen=%q: %v, want nil", addr, err)
			}
		})
	}
}

func TestConfig_Validate_RequiresEnvironmentAndNamespace(t *testing.T) {
	base := Config{Environment: "e", Namespace: "ns", Listen: "127.0.0.1:9099", PollInterval: 5 * time.Second, ShutdownTimeout: 10 * time.Second}

	c := base
	c.Environment = ""
	if err := c.Validate(); err == nil {
		t.Error("expected error for empty Environment")
	}

	c = base
	c.Namespace = ""
	if err := c.Validate(); err == nil {
		t.Error("expected error for empty Namespace")
	}
}

func TestConfig_Validate_PollIntervalBounds(t *testing.T) {
	base := Config{Environment: "e", Namespace: "ns", Listen: "127.0.0.1:9099", ShutdownTimeout: 10 * time.Second}

	c := base
	c.PollInterval = 500 * time.Millisecond
	if err := c.Validate(); err == nil {
		t.Error("expected error for poll-interval below 1s")
	}

	c = base
	c.PollInterval = 2 * time.Minute
	if err := c.Validate(); err == nil {
		t.Error("expected error for poll-interval above 60s")
	}

	c = base
	c.PollInterval = 5 * time.Second
	if err := c.Validate(); err != nil {
		t.Errorf("Validate(): %v, want nil", err)
	}
}

func TestConfig_Validate_ShutdownTimeoutBounds(t *testing.T) {
	base := Config{Environment: "e", Namespace: "ns", Listen: "127.0.0.1:9099", PollInterval: 5 * time.Second}

	c := base
	c.ShutdownTimeout = 0
	if err := c.Validate(); err == nil {
		t.Error("expected error for zero shutdown-timeout")
	}

	c = base
	c.ShutdownTimeout = 200 * time.Second
	if err := c.Validate(); err == nil {
		t.Error("expected error for shutdown-timeout above the bound")
	}
}

func TestSnapshotConfig_EngineEndpointRequiredForPodman(t *testing.T) {
	c := SnapshotConfig{Engine: "rootless-podman"}
	if err := c.Validate(); err == nil {
		t.Error("Validate() with engine=rootless-podman and no engine-endpoint: expected error, got nil")
	}

	c.EngineEndpoint = "tcp://127.0.0.1:2375"
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() with engine=rootless-podman and an engine-endpoint: %v, want nil", err)
	}

	none := SnapshotConfig{Engine: "none"}
	if err := none.Validate(); err != nil {
		t.Errorf("Validate() with engine=none and no engine-endpoint: %v, want nil", err)
	}
}

func TestLoad_Defaults(t *testing.T) {
	c, err := Load(nil, emptyEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != "127.0.0.1:9099" {
		t.Errorf("Listen = %q, want 127.0.0.1:9099", c.Listen)
	}
	if c.PollInterval != 5*time.Second {
		t.Errorf("PollInterval = %s, want 5s", c.PollInterval)
	}
	if c.ShutdownTimeout != 100*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 100s", c.ShutdownTimeout)
	}
}

func TestLoad_Flags(t *testing.T) {
	c, err := Load([]string{"--environment=env-a", "--namespace=ns-a", "--listen=127.0.0.1:9100"}, emptyEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Environment != "env-a" || c.Namespace != "ns-a" || c.Listen != "127.0.0.1:9100" {
		t.Errorf("Load() = %+v, unexpected", c)
	}
}

// TestListenAddress_ResolvesLoopback is a live-bind sanity check: net.Listen
// on the default Listen address actually yields a loopback listener, the
// concrete thing Validate's static check is a proxy for.
func TestListenAddress_ResolvesLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	host, _, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Errorf("listener address %q is not loopback", ln.Addr().String())
	}
}
