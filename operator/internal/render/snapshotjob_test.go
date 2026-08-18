package render

import (
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

func TestRenderSnapshotJob_ErrorsWithoutS3Backend(t *testing.T) {
	class := withEngine(pvcBackendClass(), v1alpha1.EngineTypeNone)
	in := Inputs{Env: baseEnv("pvc-minimal"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}
	if _, err := RenderSnapshotJob(in); err == nil {
		t.Error("RenderSnapshotJob with a pvc backend: expected error, got nil")
	}
}
