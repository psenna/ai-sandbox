package render

import (
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// TestRender_Deterministic calls Render with identical Inputs 100 times and
// asserts every result marshals to the byte-identical output as the first,
// catching accidental Go map-iteration-order leaking into the rendered
// objects.
func TestRender_Deterministic(t *testing.T) {
	in := Inputs{
		Env:         baseEnv("determinism-env"),
		Class:       fullClass(),
		Credentials: Credentials{GitProxyToken: "fake-token-determinism"},
		ClusterID:   "test-cluster",
	}

	first, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := marshalAll(t, first)

	for i := 0; i < 100; i++ {
		got, err := Render(in)
		if err != nil {
			t.Fatalf("Render iteration %d: %v", i, err)
		}
		gotBytes := marshalAll(t, got)
		if want != gotBytes {
			t.Fatalf("Render is not deterministic at iteration %d", i)
		}
	}
}

// TestRenderPod_Deterministic mirrors TestRender_Deterministic for the pod
// renderer. engine: none is used because the CRD-default engine
// (rootless-podman) is deliberately unimplemented and errors (#24).
func TestRenderPod_Deterministic(t *testing.T) {
	in := Inputs{
		Env:          baseEnv("determinism-pod-env"),
		Class:        withEngine(fullClass(), v1alpha1.EngineTypeNone),
		Credentials:  Credentials{GitProxyToken: "fake-token-determinism-pod"}, //nolint:gosec // G101: deliberately fake test fixture value, not a real credential
		ClusterID:    "test-cluster",
		SidecarImage: "test-sidecar:test",
	}

	first, err := RenderPod(in)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	want := marshalForGolden(t, first)

	for i := 0; i < 100; i++ {
		got, err := RenderPod(in)
		if err != nil {
			t.Fatalf("RenderPod iteration %d: %v", i, err)
		}
		gotBytes := marshalForGolden(t, got)
		if string(want) != string(gotBytes) {
			t.Fatalf("RenderPod is not deterministic at iteration %d", i)
		}
	}
}

// TestRenderPod_PodmanDeterministic mirrors TestRenderPod_Deterministic for
// the rootless-podman engine -- catching Go map-iteration order leaking
// through podmanRegistriesConf (the registry-mirror rendering iterates a
// dedupe map before sorting).
func TestRenderPod_PodmanDeterministic(t *testing.T) {
	class := fullClass()
	class.Spec.Engine.Type = v1alpha1.EngineTypeRootlessPodman
	class.Spec.Services.RegistryMirror = &v1alpha1.RegistryMirrorService{
		URL:        "http://registry-cache.ai-sandbox-e2e.svc.cluster.local:5000",
		Registries: []string{"docker.io", "quay.io", "ghcr.io"},
	}
	in := Inputs{
		Env:          baseEnv("determinism-pod-podman"),
		Class:        class,
		Credentials:  Credentials{GitProxyToken: "fake-token-determinism-podman"}, //nolint:gosec // G101: deliberately fake test fixture value, not a real credential
		ClusterID:    "test-cluster",
		SidecarImage: "test-sidecar:test",
	}

	first, err := RenderPod(in)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	want := marshalForGolden(t, first)

	for i := 0; i < 100; i++ {
		got, err := RenderPod(in)
		if err != nil {
			t.Fatalf("RenderPod iteration %d: %v", i, err)
		}
		gotBytes := marshalForGolden(t, got)
		if string(want) != string(gotBytes) {
			t.Fatalf("RenderPod (podman) is not deterministic at iteration %d", i)
		}
	}
}

func marshalAll(t *testing.T, objs Objects) string {
	t.Helper()
	var b []byte
	b = append(b, marshalForGolden(t, objs.ServiceAccount)...)
	b = append(b, marshalForGolden(t, objs.Role)...)
	b = append(b, marshalForGolden(t, objs.RoleBinding)...)
	b = append(b, marshalForGolden(t, objs.PVC)...)
	b = append(b, marshalForGolden(t, objs.ConfigMap)...)
	b = append(b, marshalForGolden(t, objs.Secret)...)
	return string(b)
}
