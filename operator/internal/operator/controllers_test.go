package operator

import (
	"testing"

	"k8s.io/client-go/rest"
)

// TestSetupControllers_RegistersRunnables proves SetupControllers (which
// registers #20's SlotScheduler and #29's WarmCacheGC via mgr.Add) still
// succeeds without live cluster connectivity, preserving the "manager
// construction must not require connectivity" contract manager.go's
// comments defend: it reuses TestNew_ConstructsWithoutCluster's fake,
// unreachable host.
func TestSetupControllers_RegistersRunnables(t *testing.T) {
	restCfg := &rest.Config{Host: "https://127.0.0.1:6443"}
	cfg := baseConfig()

	mgr, err := New(restCfg, cfg)
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}

	if err := SetupControllers(mgr, cfg); err != nil {
		t.Fatalf("SetupControllers() returned error: %v", err)
	}
}
