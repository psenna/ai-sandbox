package render

import (
	"errors"
	"strings"
	"testing"

	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestRenderPod_RootlessPodmanIsNotImplemented(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeRootlessPodman)
	in := Inputs{Env: baseEnv("engine-podman"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}

	_, err := RenderPod(in)
	if err == nil {
		t.Fatal("RenderPod with engine: rootless-podman: expected error, got nil")
	}
	if !errors.Is(err, ErrEngineNotImplemented) {
		t.Errorf("RenderPod error = %v, want errors.Is(err, ErrEngineNotImplemented)", err)
	}
	if !strings.Contains(err.Error(), "#24") {
		t.Errorf("RenderPod error = %q, want it to name issue #24", err.Error())
	}
}

func TestRenderPod_EmptyEngineTypeDefaultsToRootlessPodman(t *testing.T) {
	class := minimalClass() // Spec.Engine.Type left as the zero value ""
	in := Inputs{Env: baseEnv("engine-default"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}

	_, err := RenderPod(in)
	if err == nil {
		t.Fatal("RenderPod with engine.type \"\": expected error (defaults to unimplemented rootless-podman), got nil")
	}
	if !errors.Is(err, ErrEngineNotImplemented) {
		t.Errorf("RenderPod error = %v, want errors.Is(err, ErrEngineNotImplemented)", err)
	}
}

func TestRenderPod_UnknownEngineType(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineType("docker"))
	in := Inputs{Env: baseEnv("engine-unknown"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}

	_, err := RenderPod(in)
	if err == nil {
		t.Fatal("RenderPod with engine.type \"docker\": expected error, got nil")
	}
	if errors.Is(err, ErrEngineNotImplemented) {
		t.Errorf("RenderPod error = %v, want a distinct \"unknown engine type\" error, not ErrEngineNotImplemented", err)
	}
	if !strings.Contains(err.Error(), "unknown engine type") {
		t.Errorf("RenderPod error = %q, want it to mention \"unknown engine type\"", err.Error())
	}
}

func TestNoneEngine_Contribute(t *testing.T) {
	c, err := noneEngine{}.Contribute(Inputs{})
	if err != nil {
		t.Fatalf("noneEngine.Contribute: %v", err)
	}
	if len(c.Containers) != 0 || len(c.InitContainers) != 0 || len(c.Volumes) != 0 ||
		len(c.AgentEnv) != 0 || len(c.AgentVolumeMounts) != 0 || len(c.Relaxations) != 0 {
		t.Errorf("noneEngine.Contribute() = %+v, want zero Contribution", c)
	}
	if got := (noneEngine{}).Type(); got != v1alpha1.EngineTypeNone {
		t.Errorf("noneEngine.Type() = %q, want %q", got, v1alpha1.EngineTypeNone)
	}
}

func TestEngineRelaxations(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		relaxations, ok := EngineRelaxations(v1alpha1.EngineTypeNone)
		if !ok {
			t.Fatal("EngineRelaxations(none) ok = false, want true")
		}
		if relaxations != nil {
			t.Errorf("EngineRelaxations(none) = %v, want nil", relaxations)
		}
	})

	t.Run("rootless-podman", func(t *testing.T) {
		_, ok := EngineRelaxations(v1alpha1.EngineTypeRootlessPodman)
		if ok {
			t.Error("EngineRelaxations(rootless-podman) ok = true, want false (not implemented)")
		}
	})

	t.Run("unknown", func(t *testing.T) {
		_, ok := EngineRelaxations(v1alpha1.EngineType("docker"))
		if ok {
			t.Error("EngineRelaxations(docker) ok = true, want false (unknown)")
		}
	})
}

// fakeRelaxingEngine is a test-only Engine that requests one Relaxation of
// every RelaxationKind, proving RenderPod applies each correctly.
type fakeRelaxingEngine struct {
	relaxations []Relaxation
}

func (fakeRelaxingEngine) Type() v1alpha1.EngineType { return v1alpha1.EngineType("fake-relaxing") }

func (e fakeRelaxingEngine) Contribute(Inputs) (Contribution, error) {
	return Contribution{Relaxations: e.relaxations}, nil
}

func withFakeEngine(t *testing.T, typ v1alpha1.EngineType, e Engine) func() {
	t.Helper()
	prev, existed := engineRegistry[typ]
	engineRegistry[typ] = e
	return func() {
		if existed {
			engineRegistry[typ] = prev
		} else {
			delete(engineRegistry, typ)
		}
	}
}

func TestRenderPod_AppliesEveryRelaxationKind(t *testing.T) {
	const fakeType = v1alpha1.EngineType("fake-relaxing")
	relaxations := []Relaxation{
		{Container: AgentContainerName, Kind: RelaxAppArmorUnconfined, Reason: "test: apparmor"},
		{Container: AgentContainerName, Kind: RelaxSeccompUnset, Reason: "test: seccomp"},
		{Container: AgentContainerName, Kind: RelaxAllowPrivilegeEscalation, Reason: "test: privesc"},
		{Container: AgentContainerName, Kind: RelaxAddCapability, Value: "SETUID", Reason: "test: setuid"},
	}
	defer withFakeEngine(t, fakeType, fakeRelaxingEngine{relaxations: relaxations})()

	class := withEngine(minimalClass(), fakeType)
	pod, err := RenderPod(Inputs{Env: baseEnv("engine-relax"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}

	sc := findAgentSecurityContext(t, pod)
	if sc.AppArmorProfile == nil || sc.AppArmorProfile.Type == nil || *sc.AppArmorProfile.Type != "Unconfined" {
		t.Errorf("AppArmorProfile = %+v, want type Unconfined", sc.AppArmorProfile)
	}
	if sc.SeccompProfile != nil {
		t.Errorf("SeccompProfile = %+v, want nil (unset)", sc.SeccompProfile)
	}
	if sc.AllowPrivilegeEscalation == nil || !*sc.AllowPrivilegeEscalation {
		t.Errorf("AllowPrivilegeEscalation = %v, want true", sc.AllowPrivilegeEscalation)
	}
	if sc.Capabilities == nil {
		t.Fatal("Capabilities is nil, want Add to contain SETUID")
	}
	found := false
	for _, c := range sc.Capabilities.Add {
		if string(c) == "SETUID" {
			found = true
		}
	}
	if !found {
		t.Errorf("Capabilities.Add = %v, want to contain SETUID", sc.Capabilities.Add)
	}
	// The drop: [ALL] baseline must survive relaxation.
	dropped := false
	for _, c := range sc.Capabilities.Drop {
		if string(c) == "ALL" {
			dropped = true
		}
	}
	if !dropped {
		t.Errorf("Capabilities.Drop = %v, want to still contain ALL", sc.Capabilities.Drop)
	}
}

func TestRenderPod_RelaxationErrors(t *testing.T) {
	const fakeType = v1alpha1.EngineType("fake-relaxing")

	cases := []struct {
		name        string
		relaxations []Relaxation
		wantSubstr  string
	}{
		{
			name:        "unknown kind",
			relaxations: []Relaxation{{Container: AgentContainerName, Kind: RelaxationKind("Bogus"), Reason: "x"}},
			wantSubstr:  "unknown relaxation kind",
		},
		{
			name:        "empty reason",
			relaxations: []Relaxation{{Container: AgentContainerName, Kind: RelaxSeccompUnset, Reason: ""}},
			wantSubstr:  "no Reason",
		},
		{
			name:        "unknown container",
			relaxations: []Relaxation{{Container: "does-not-exist", Kind: RelaxSeccompUnset, Reason: "x"}},
			wantSubstr:  "not in the rendered pod",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer withFakeEngine(t, fakeType, fakeRelaxingEngine{relaxations: tc.relaxations})()
			class := withEngine(minimalClass(), fakeType)
			_, err := RenderPod(Inputs{Env: baseEnv("engine-relax-err"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
			if err == nil {
				t.Fatal("RenderPod: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("RenderPod error = %q, want substring %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func findAgentSecurityContext(t *testing.T, pod *acorev1.PodApplyConfiguration) *acorev1.SecurityContextApplyConfiguration {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name != nil && *c.Name == AgentContainerName {
			if c.SecurityContext == nil {
				t.Fatal("agent container has no SecurityContext")
			}
			return c.SecurityContext
		}
	}
	t.Fatal("agent container not found in rendered pod")
	return nil
}
