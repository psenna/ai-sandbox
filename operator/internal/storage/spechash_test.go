package storage

import (
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func baseClassSpec() *v1alpha1.SandboxClassSpec {
	return &v1alpha1.SandboxClassSpec{
		Agent: v1alpha1.AgentSpec{Image: "ghcr.io/example/agent:v1"},
		Storage: v1alpha1.StorageSpec{
			Backend: v1alpha1.BackendSpec{Type: v1alpha1.StorageBackendTypeS3},
		},
	}
}

func baseEnvSpec() *v1alpha1.SandboxEnvironmentSpec {
	return &v1alpha1.SandboxEnvironmentSpec{
		ClassRef: v1alpha1.ClassRef{Name: "class-a"},
		Repo:     "owner/repo",
		Task:     v1alpha1.TaskSpec{Prompt: "do the thing"},
	}
}

func TestSpecHash_IdenticalSpecsProduceIdenticalHash(t *testing.T) {
	h1, err := SpecHash(baseClassSpec(), baseEnvSpec())
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	h2, err := SpecHash(baseClassSpec(), baseEnvSpec())
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	if h1 != h2 {
		t.Errorf("identical specs produced different hashes: %q vs %q", h1, h2)
	}
	if h1 == "" {
		t.Error("hash must not be empty")
	}
}

func TestSpecHash_ChangedFieldProducesDifferentHash(t *testing.T) {
	base, err := SpecHash(baseClassSpec(), baseEnvSpec())
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}

	changedClass := baseClassSpec()
	changedClass.Agent.Image = "ghcr.io/example/agent:v2"
	got, err := SpecHash(changedClass, baseEnvSpec())
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	if got == base {
		t.Error("changing class.agent.image did not change the hash")
	}

	changedEnv := baseEnvSpec()
	changedEnv.Repo = "owner/other-repo"
	got2, err := SpecHash(baseClassSpec(), changedEnv)
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	if got2 == base {
		t.Error("changing env.repo did not change the hash")
	}
}

func TestSpecHash_NilInputsAreInvalid(t *testing.T) {
	if _, err := SpecHash(nil, baseEnvSpec()); !IsInvalid(err) {
		t.Errorf("SpecHash(nil, env): want ErrInvalid, got %v", err)
	}
	if _, err := SpecHash(baseClassSpec(), nil); !IsInvalid(err) {
		t.Errorf("SpecHash(class, nil): want ErrInvalid, got %v", err)
	}
}
