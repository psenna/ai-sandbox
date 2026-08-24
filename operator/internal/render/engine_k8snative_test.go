package render

import (
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestK8sNativeEngineRegisteredAndNoop(t *testing.T) {
	e, err := engineFor(v1alpha1.EngineTypeK8sNative)
	if err != nil {
		t.Fatalf("engineFor(k8s-native): %v", err)
	}
	if e.Type() != v1alpha1.EngineTypeK8sNative {
		t.Fatalf("Type() = %q, want %q", e.Type(), v1alpha1.EngineTypeK8sNative)
	}
	c, err := e.Contribute(Inputs{})
	if err != nil {
		t.Fatalf("Contribute: %v", err)
	}
	if len(c.Containers) != 0 || len(c.Volumes) != 0 || len(c.Relaxations) != 0 {
		t.Fatalf("Contribute not a no-op: %+v", c)
	}
	// EngineRelaxations must report the engine as knowable AND relaxed=false.
	relaxations, ok := EngineRelaxations(v1alpha1.EngineTypeK8sNative)
	if !ok {
		t.Fatal("EngineRelaxations(k8s-native) ok=false, want true (engine is implemented)")
	}
	if len(relaxations) != 0 {
		t.Fatalf("EngineRelaxations(k8s-native) = %v, want empty (k8s-native needs no relaxations)", relaxations)
	}
}
