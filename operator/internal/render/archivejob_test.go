package render

import (
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestRenderArchiveJob_NoneMinimal(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	in := Inputs{Env: baseEnv("none-minimal"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}
	job, err := RenderArchiveJob(in)
	if err != nil {
		t.Fatalf("RenderArchiveJob: %v", err)
	}
	assertGolden(t, "none-minimal.archivejob.yaml", marshalForGolden(t, job))

	names := ChildNames("none-minimal")
	if job.Name == nil || *job.Name != names.ArchiveJob {
		t.Errorf("job.Name = %v, want %q", job.Name, names.ArchiveJob)
	}
	if job.Spec == nil || job.Spec.Template == nil || job.Spec.Template.Spec == nil {
		t.Fatal("job.Spec.Template.Spec is nil")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != archiveJobBackoffLimit {
		t.Errorf("BackoffLimit = %v, want %d", job.Spec.BackoffLimit, archiveJobBackoffLimit)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != archiveJobTTLSecondsAfterFinished {
		t.Errorf("TTLSecondsAfterFinished = %v, want %d", job.Spec.TTLSecondsAfterFinished, archiveJobTTLSecondsAfterFinished)
	}

	foundArchiveArg := false
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, a := range c.Args {
			if a == "archive" {
				foundArchiveArg = true
			}
		}
		for _, v := range c.VolumeMounts {
			if v.Name != nil && (*v.Name == workspaceVolumeName || *v.Name == agentHomeVolumeName) {
				t.Errorf("archive job must not mount volume %q (it touches no PVC)", *v.Name)
			}
		}
	}
	if !foundArchiveArg {
		t.Error(`expected "archive" as the sandboxctl subcommand in the archive job's args`)
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name != nil && (*v.Name == workspaceVolumeName || *v.Name == agentHomeVolumeName || *v.Name == configVolumeName) {
			t.Errorf("archive job must not declare volume %q", *v.Name)
		}
	}
}

func TestRenderArchiveJob_ErrorsWithoutS3Backend(t *testing.T) {
	class := withEngine(pvcBackendClass(), v1alpha1.EngineTypeNone)
	in := Inputs{Env: baseEnv("pvc-minimal"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}
	if _, err := RenderArchiveJob(in); err == nil {
		t.Error("RenderArchiveJob with a pvc backend: expected error, got nil")
	}
}

func TestRenderArchiveJob_ErrorsWithoutSidecarImage(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	in := Inputs{Env: baseEnv("none-minimal"), Class: class, ClusterID: "test-cluster"}
	if _, err := RenderArchiveJob(in); err == nil {
		t.Error("RenderArchiveJob without SidecarImage: expected error, got nil")
	}
}

// TestRenderArchiveJob_Deterministic proves RenderArchiveJob is pure: two
// calls with the same Inputs render byte-identical output.
func TestRenderArchiveJob_Deterministic(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	in := Inputs{Env: baseEnv("none-minimal"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}

	a, err := RenderArchiveJob(in)
	if err != nil {
		t.Fatalf("RenderArchiveJob (1st): %v", err)
	}
	b, err := RenderArchiveJob(in)
	if err != nil {
		t.Fatalf("RenderArchiveJob (2nd): %v", err)
	}
	if string(marshalForGolden(t, a)) != string(marshalForGolden(t, b)) {
		t.Error("RenderArchiveJob is not deterministic across two calls with identical Inputs")
	}
}
