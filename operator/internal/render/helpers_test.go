package render

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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
	return c
}
