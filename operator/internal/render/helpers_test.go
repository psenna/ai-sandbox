package render

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// fixedUID is used by every test that needs a stable, deterministic UID.
const fixedUID = "11111111-1111-1111-1111-111111111111"

// envOpt mutates a test SandboxEnvironment.
type envOpt func(*v1alpha1.SandboxEnvironment)

func withPrompt(prompt string) envOpt {
	return func(e *v1alpha1.SandboxEnvironment) {
		e.Spec.Task = v1alpha1.TaskSpec{Prompt: prompt}
	}
}

func withIssueRef(repo string, number int32) envOpt {
	return func(e *v1alpha1.SandboxEnvironment) {
		e.Spec.Task = v1alpha1.TaskSpec{IssueRef: &v1alpha1.IssueRef{Repo: repo, Number: number}}
	}
}

// baseEnv builds a minimal, deterministic SandboxEnvironment named name with
// a fixed UID.
func baseEnv(name string, opts ...envOpt) *v1alpha1.SandboxEnvironment {
	e := &v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(fixedUID),
		},
		Spec: v1alpha1.SandboxEnvironmentSpec{
			ClassRef: v1alpha1.ClassRef{Name: "default"},
			Repo:     "org/repo",
			Task:     v1alpha1.TaskSpec{Prompt: "do the thing"},
		},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// minimalClass mirrors the minimal SandboxClass shape used in
// internal/controller/suite_test.go's mustCreateClass: no services, no
// model, empty/defaulted storage.
func minimalClass() *v1alpha1.SandboxClass {
	return &v1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: v1alpha1.SandboxClassSpec{
			Agent: v1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Storage: v1alpha1.StorageSpec{
				Backend: v1alpha1.BackendSpec{
					Type: v1alpha1.StorageBackendTypeS3,
					S3: &v1alpha1.S3Backend{
						Endpoint: "https://s3.example.com",
						Bucket:   "sandbox-snapshots",
						CredentialsSecretRef: v1alpha1.SecretKeyRef{
							Name: "s3-creds",
						},
					},
				},
			},
		},
	}
}

// fullClass builds a SandboxClass with every service block, all four model
// tiers, and explicit storage settings.
func fullClass() *v1alpha1.SandboxClass {
	c := minimalClass()
	c.Spec.Agent.Model = &v1alpha1.ModelSpec{
		Default: "glm-5.2:cloud",
		Haiku:   "deepseek-v4-flash:0731-cloud",
		Sonnet:  "deepseek-v4-flash:0731-cloud",
		Opus:    "glm-5.2:cloud",
	}
	c.Spec.Services = v1alpha1.ServicesSpec{
		GitProxy: &v1alpha1.GitProxyService{
			GitURL:    "http://git-proxy:8080",
			BrokerURL: "http://git-proxy:8090",
			TokenSecretRef: v1alpha1.SecretKeyRef{
				Name: "git-proxy-token",
			},
		},
		DependaProxy: &v1alpha1.DependaProxyService{
			NpmURL:     "http://dependaproxy:8080/npm",
			PypiURL:    "http://dependaproxy:8080/pypi",
			GoproxyURL: "http://dependaproxy:8080/goproxy",
		},
		Ollama: &v1alpha1.OllamaService{
			BaseURL: "http://ollama:11434",
		},
	}
	scn := "fast-ssd"
	c.Spec.Storage.Workspace = v1alpha1.WorkspaceSpec{
		Size:             "50Gi",
		StorageClassName: &scn,
	}
	c.Spec.Agent.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
	return c
}

// pvcBackendClass mirrors minimalClass but with a PVC snapshot storage
// backend, used to prove no S3/snapshot-credentials wiring is rendered for
// a non-S3 backend (#28).
func pvcBackendClass() *v1alpha1.SandboxClass {
	return &v1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: v1alpha1.SandboxClassSpec{
			Agent: v1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Storage: v1alpha1.StorageSpec{
				Backend: v1alpha1.BackendSpec{
					Type: v1alpha1.StorageBackendTypePVC,
					PVC:  &v1alpha1.PVCBackend{ClaimName: "snapshots-pvc"},
				},
			},
		},
	}
}

// withEngine sets class.Spec.Engine.Type to t, returning class for chaining.
func withEngine(class *v1alpha1.SandboxClass, t v1alpha1.EngineType) *v1alpha1.SandboxClass {
	class.Spec.Engine.Type = t
	return class
}

// podmanClass returns minimalClass with engine.type=rootless-podman and a
// declared registry mirror.
func podmanClass() *v1alpha1.SandboxClass {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeRootlessPodman)
	class.Spec.Services.RegistryMirror = &v1alpha1.RegistryMirrorService{
		URL: "http://registry-cache.ai-sandbox-e2e.svc.cluster.local:5000",
	}
	return class
}

// podmanContainerOf finds the `podman` init container in a rendered pod.
func podmanContainerOf(t *testing.T, pod *acorev1.PodApplyConfiguration) *acorev1.ContainerApplyConfiguration {
	t.Helper()
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if c.Name != nil && *c.Name == PodmanContainerName {
			return c
		}
	}
	t.Fatal("podman init container not found in rendered pod")
	return nil
}
