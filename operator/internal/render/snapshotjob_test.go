package render

import (
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestRenderSnapshotJob_NoneMinimal(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	in := Inputs{Env: baseEnv("none-minimal"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}
	job, err := RenderSnapshotJob(in)
	if err != nil {
		t.Fatalf("RenderSnapshotJob: %v", err)
	}
	assertGolden(t, "none-minimal.snapshotjob.yaml", marshalForGolden(t, job))

	names := ChildNames("none-minimal")
	if job.Name == nil || *job.Name != names.SnapshotJob {
		t.Errorf("job.Name = %v, want %q", job.Name, names.SnapshotJob)
	}
	if job.Spec == nil || job.Spec.Template == nil || job.Spec.Template.Spec == nil {
		t.Fatal("job.Spec.Template.Spec is nil")
	}
	foundAgentHomeEmpty := false
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, a := range c.Args {
			if a == "--agent-home-path=" {
				foundAgentHomeEmpty = true
			}
		}
		for _, v := range c.VolumeMounts {
			if v.Name != nil && *v.Name == agentHomeVolumeName {
				t.Error("snapshot job must not mount the agent-home volume (it never had one)")
			}
		}
	}
	if !foundAgentHomeEmpty {
		t.Error("expected --agent-home-path= (empty) in the snapshot job's args")
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name != nil && *v.Name == agentHomeVolumeName {
			t.Error("snapshot job must not declare an agent-home volume")
		}
		if v.Name != nil && *v.Name == configVolumeName {
			t.Error("snapshot job must not declare a config volume")
		}
	}
}

// TestRenderSnapshotJob_ForcesEngineNone proves the recovery Job always
// renders --engine=none, even when the class's engine is rootless-podman:
// the recovery Job pod has no podman sidecar at all (#24), so there is no
// engine API to dial and, by construction, no workload container to tear
// down.
func TestRenderSnapshotJob_ForcesEngineNone(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeRootlessPodman)
	in := Inputs{Env: baseEnv("podman-snapshotjob"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}
	job, err := RenderSnapshotJob(in)
	if err != nil {
		t.Fatalf("RenderSnapshotJob: %v", err)
	}
	if job.Spec == nil || job.Spec.Template == nil || job.Spec.Template.Spec == nil {
		t.Fatal("job.Spec.Template.Spec is nil")
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		lastEngine := ""
		for _, a := range c.Args {
			if a == "--engine=none" {
				lastEngine = "none"
			} else if strings.HasPrefix(a, "--engine=") {
				lastEngine = a
			}
		}
		if lastEngine != "none" {
			t.Errorf("last --engine= flag = %q, want \"none\" (flag.FlagSet's last-flag-wins semantics); args=%v", lastEngine, c.Args)
		}
	}
}

func TestRenderSnapshotJob_ErrorsWithoutS3Backend(t *testing.T) {
	class := withEngine(pvcBackendClass(), v1alpha1.EngineTypeNone)
	in := Inputs{Env: baseEnv("pvc-minimal"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}
	if _, err := RenderSnapshotJob(in); err == nil {
		t.Error("RenderSnapshotJob with a pvc backend: expected error, got nil")
	}
}
