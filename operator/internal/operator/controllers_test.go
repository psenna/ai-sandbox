package operator

import (
	"testing"

	"k8s.io/client-go/rest"
)

// TestSetupControllers_RegistersSchedulerRunnable proves SetupControllers
// (which now also registers #20's SlotScheduler via mgr.Add) still
// succeeds without live cluster connectivity, preserving the "manager
// construction must not require connectivity" contract manager.go's
// comments defend: it reuses TestNew_ConstructsWithoutCluster's fake,
// unreachable host.
func TestSetupControllers_RegistersSchedulerRunnable(t *testing.T) {
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
