package render

import (
	"testing"

	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func restorePlan() *RestorePlan {
	return &RestorePlan{SnapshotID: "00003-2026-01-01T00:00:00Z", Seq: 3}
}

// TestRenderPod_NoneRestore is the new golden fixture: a pod WITH
// in.Restore set against an S3-backed class. Every EXISTING none-*.pod.yaml
// golden must stay byte-identical (in.Restore is nil there) -- verified by
// the unchanged TestRenderPod_NoneMinimal/NoneFull/NoneLongName passing
// without -update.
func TestRenderPod_NoneRestore(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	in := Inputs{
		Env: baseEnv("none-restore"), Class: class, ClusterID: "test-cluster",
		SidecarImage: "test-sidecar:test", Restore: restorePlan(),
	}
	assertGoldenPod(t, "none-restore", in)
}

// TestRenderPod_RestoreContainer_PresentOnlyWithRestorePlanAndS3Backend
// proves the restore init container is rendered exactly when in.Restore !=
// nil AND the class's storage backend is S3, and absent otherwise.
func TestRenderPod_RestoreContainer_PresentOnlyWithRestorePlanAndS3Backend(t *testing.T) {
	hasRestore := func(t *testing.T, in Inputs) bool {
		t.Helper()
		pod, err := RenderPod(in)
		if err != nil {
			t.Fatalf("RenderPod: %v", err)
		}
		for _, c := range pod.Spec.InitContainers {
			if c.Name != nil && *c.Name == RestoreContainerName {
				return true
			}
		}
		return false
	}

	t.Run("no restore plan, S3 backend: absent", func(t *testing.T) {
		class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
		in := Inputs{Env: baseEnv("no-restore-s3"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}
		if hasRestore(t, in) {
			t.Error("restore container present with in.Restore == nil, want absent")
		}
	})

	t.Run("restore plan, S3 backend: present", func(t *testing.T) {
		class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
		in := Inputs{Env: baseEnv("restore-s3"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test", Restore: restorePlan()}
		if !hasRestore(t, in) {
			t.Error("restore container absent with in.Restore set and S3 backend, want present")
		}
	})

	t.Run("restore plan, pvc backend: absent", func(t *testing.T) {
		class := withEngine(pvcBackendClass(), v1alpha1.EngineTypeNone)
		in := Inputs{Env: baseEnv("restore-pvc"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test", Restore: restorePlan()}
		if hasRestore(t, in) {
			t.Error("restore container present for a pvc-backend class, want absent (restore has no pvc support, Q7)")
		}
	})
}

// TestRenderPod_RestoreContainer_OrderingAndShape proves: sandboxctl first,
// restore last among init containers, and the restore container has no
// restartPolicy set (a plain, one-shot init container, never restartPolicy:
// Always).
func TestRenderPod_RestoreContainer_OrderingAndShape(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	in := Inputs{Env: baseEnv("restore-order"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test", Restore: restorePlan()}
	pod, err := RenderPod(in)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}

	ics := pod.Spec.InitContainers
	if len(ics) < 2 {
		t.Fatalf("expected at least 2 init containers (sandboxctl, restore), got %d", len(ics))
	}
	if ics[0].Name == nil || *ics[0].Name != SidecarContainerName {
		t.Errorf("first init container = %v, want %q", ics[0].Name, SidecarContainerName)
	}
	last := ics[len(ics)-1]
	if last.Name == nil || *last.Name != RestoreContainerName {
		t.Errorf("last init container = %v, want %q", last.Name, RestoreContainerName)
	}
	if last.RestartPolicy != nil {
		t.Errorf("restore container RestartPolicy = %v, want unset (a plain, one-shot init container)", *last.RestartPolicy)
	}

	// The sidecar itself must still be the restartable native sidecar.
	if ics[0].RestartPolicy == nil {
		t.Error("sandboxctl container RestartPolicy is unset, want restartPolicy: Always")
	}
}

// TestRenderPod_RestoreContainer_Mounts proves the exact mount set: token
// (read-only), workspace, agent-home, snapshot-credentials (read-only).
func TestRenderPod_RestoreContainer_Mounts(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	in := Inputs{Env: baseEnv("restore-mounts"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test", Restore: restorePlan()}
	pod, err := RenderPod(in)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}

	var restore *struct {
		mounts map[string]bool
		ro     map[string]bool
	}
	for _, c := range pod.Spec.InitContainers {
		if c.Name == nil || *c.Name != RestoreContainerName {
			continue
		}
		restore = &struct {
			mounts map[string]bool
			ro     map[string]bool
		}{mounts: map[string]bool{}, ro: map[string]bool{}}
		for _, m := range c.VolumeMounts {
			if m.Name == nil {
				continue
			}
			restore.mounts[*m.Name] = true
			restore.ro[*m.Name] = m.ReadOnly != nil && *m.ReadOnly
		}
	}
	if restore == nil {
		t.Fatal("restore container not found")
	}

	want := []string{sidecarTokenVolumeName, workspaceVolumeName, agentHomeVolumeName, snapshotCredentialsVolumeName}
	for _, name := range want {
		if !restore.mounts[name] {
			t.Errorf("restore container missing mount %q", name)
		}
	}
	if len(restore.mounts) != len(want) {
		t.Errorf("restore container mounts = %v, want exactly %v", restore.mounts, want)
	}
	if !restore.ro[sidecarTokenVolumeName] {
		t.Error("token mount must be read-only")
	}
	if !restore.ro[snapshotCredentialsVolumeName] {
		t.Error("snapshot-credentials mount must be read-only")
	}
}

// TestRenderPod_RestoreContainer_ReservedNameCollision proves an engine
// contribution naming a container "restore" is rejected, matching the
// existing agent/sandboxctl collision guard -- validateNoReservedContainerNames
// is exercised directly (as RenderPod itself does) since no engine
// implementation in this repo actually contributes containers today.
func TestRenderPod_RestoreContainer_ReservedNameCollision(t *testing.T) {
	name := RestoreContainerName
	c := Contribution{InitContainers: []*acorev1.ContainerApplyConfiguration{
		acorev1.Container().WithName(name),
	}}
	if err := validateNoReservedContainerNames(c); err == nil {
		t.Fatal("validateNoReservedContainerNames: want error for a container named \"restore\", got nil")
	}
}
