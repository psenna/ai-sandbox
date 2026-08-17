package render

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// assertGoldenPod renders the Pod for in and asserts it against its golden
// file under testdata/<case>.pod.yaml.
func assertGoldenPod(t *testing.T, caseName string, in Inputs) {
	t.Helper()
	pod, err := RenderPod(in)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	assertGolden(t, caseName+".pod.yaml", marshalForGolden(t, pod))
}

func TestRenderPod_NoneMinimal(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	in := Inputs{Env: baseEnv("none-minimal"), Class: class, ClusterID: "test-cluster"}
	assertGoldenPod(t, "none-minimal", in)
}

func TestRenderPod_NoneFull(t *testing.T) {
	class := withEngine(fullClass(), v1alpha1.EngineTypeNone)
	in := Inputs{
		Env:         baseEnv("none-full", withPrompt("do the full thing")),
		Class:       class,
		Credentials: Credentials{GitProxyToken: "fake-git-proxy-token-pod"}, //nolint:gosec // G101: deliberately fake test fixture value, not a real credential
		ClusterID:   "test-cluster",
	}
	assertGoldenPod(t, "none-full", in)
}

func TestRenderPod_NoneLongName(t *testing.T) {
	longName := strings.Repeat("p", 190) + "-" + strings.Repeat("q", 9) // 200 chars
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	in := Inputs{Env: baseEnv(longName), Class: class, ClusterID: "test-cluster"}
	assertGoldenPod(t, "none-long-name", in)

	pod, err := RenderPod(in)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	names := ChildNames(longName)
	if pod.Name == nil || *pod.Name != names.Pod {
		t.Errorf("pod.Name = %v, want %q", pod.Name, names.Pod)
	}
	if len(*pod.Name) > MaxNameLen {
		t.Errorf("pod name %q has length %d, want <= %d", *pod.Name, len(*pod.Name), MaxNameLen)
	}
	if pod.Spec.ServiceAccountName == nil || *pod.Spec.ServiceAccountName != names.ServiceAccount {
		t.Errorf("pod.Spec.ServiceAccountName = %v, want %q", pod.Spec.ServiceAccountName, names.ServiceAccount)
	}
	// Every volume/mount reference must use the same truncated names as the
	// other rendered child objects.
	foundPVC := false
	foundConfigMap := false
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			if v.PersistentVolumeClaim.ClaimName == nil || *v.PersistentVolumeClaim.ClaimName != names.PVC {
				t.Errorf("PVC volume claimName = %v, want %q", v.PersistentVolumeClaim.ClaimName, names.PVC)
			}
			foundPVC = true
		}
		if v.ConfigMap != nil {
			if v.ConfigMap.Name == nil || *v.ConfigMap.Name != names.ConfigMap {
				t.Errorf("ConfigMap volume name = %v, want %q", v.ConfigMap.Name, names.ConfigMap)
			}
			foundConfigMap = true
		}
	}
	if !foundPVC {
		t.Error("no PVC volume found in rendered pod")
	}
	if !foundConfigMap {
		t.Error("no ConfigMap volume found in rendered pod")
	}
	var envFromSecret bool
	for _, c := range pod.Spec.Containers {
		if c.Name == nil || *c.Name != AgentContainerName {
			continue
		}
		for _, ef := range c.EnvFrom {
			if ef.SecretRef != nil && ef.SecretRef.Name != nil && *ef.SecretRef.Name == names.Secret {
				envFromSecret = true
			}
		}
	}
	if !envFromSecret {
		t.Error("agent container envFrom does not reference the truncated Secret name")
	}
}

func TestRenderPod_RestartPolicyNever(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	pod, err := RenderPod(Inputs{Env: baseEnv("pod-restart"), Class: class, ClusterID: "test-cluster"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if pod.Spec.RestartPolicy == nil || *pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %v, want Never", pod.Spec.RestartPolicy)
	}
}

func TestRenderPod_TerminationGracePeriod(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	pod, err := RenderPod(Inputs{Env: baseEnv("pod-grace"), Class: class, ClusterID: "test-cluster"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if pod.Spec.TerminationGracePeriodSeconds == nil || *pod.Spec.TerminationGracePeriodSeconds != DefaultTerminationGracePeriodSeconds {
		t.Errorf("TerminationGracePeriodSeconds = %v, want %d", pod.Spec.TerminationGracePeriodSeconds, DefaultTerminationGracePeriodSeconds)
	}
}

func TestRenderPod_NoCommandOnlyArgs(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	pod, err := RenderPod(Inputs{Env: baseEnv("pod-cmd"), Class: class, ClusterID: "test-cluster"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != nil && *c.Name == AgentContainerName {
			if len(c.Command) != 0 {
				t.Errorf("agent container Command = %v, want empty (ENTRYPOINT must run)", c.Command)
			}
			if len(c.Args) == 0 {
				t.Error("agent container Args is empty, want the headless claude invocation")
			}
		}
	}
}

func TestRenderPod_ResourcesOmittedWhenZero(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone) // no Resources set
	pod, err := RenderPod(Inputs{Env: baseEnv("pod-no-resources"), Class: class, ClusterID: "test-cluster"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != nil && *c.Name == AgentContainerName && c.Resources != nil {
			t.Errorf("agent container Resources = %+v, want nil when class sets none", c.Resources)
		}
	}
}

func TestRenderPod_ResourcesSetWhenPresent(t *testing.T) {
	class := withEngine(fullClass(), v1alpha1.EngineTypeNone) // fullClass sets Resources
	pod, err := RenderPod(Inputs{Env: baseEnv("pod-resources"), Class: class, ClusterID: "test-cluster"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	found := false
	for _, c := range pod.Spec.Containers {
		if c.Name != nil && *c.Name == AgentContainerName {
			if c.Resources == nil {
				t.Fatal("agent container Resources is nil, want set")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("agent container not found")
	}
}

func TestRenderPod_ErrorsOnMissingInputs(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	env := baseEnv("pod-e")

	if _, err := RenderPod(Inputs{Class: class}); err == nil {
		t.Error("RenderPod with nil Env: expected error, got nil")
	}
	if _, err := RenderPod(Inputs{Env: env}); err == nil {
		t.Error("RenderPod with nil Class: expected error, got nil")
	}
	noUID := baseEnv("pod-e")
	noUID.UID = ""
	if _, err := RenderPod(Inputs{Env: noUID, Class: class}); err == nil {
		t.Error("RenderPod with empty Env.UID: expected error, got nil")
	}
}
