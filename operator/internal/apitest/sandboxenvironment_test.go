package apitest

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

const testNamespace = "default"

func validEnv(name string) *sandboxv1alpha1.SandboxEnvironment {
	return &sandboxv1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: sandboxv1alpha1.SandboxEnvironmentSpec{
			ClassRef: sandboxv1alpha1.ClassRef{Name: "some-class"},
			Repo:     "psenna/ai-sandbox",
			Task: sandboxv1alpha1.TaskSpec{
				Prompt: "implement the thing",
				IssueRef: &sandboxv1alpha1.IssueRef{
					Repo:   "psenna/ai-sandbox",
					Number: 17,
				},
			},
			Priority: 5,
			Suspend:  true,
		},
	}
}

func TestEnvRoundTripAndDefaults(t *testing.T) {
	want := validEnv("roundtrip-env")

	if err := k8s.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: want.Name, Namespace: want.Namespace}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Spec.ClassRef != want.Spec.ClassRef {
		t.Errorf("ClassRef = %+v, want %+v", got.Spec.ClassRef, want.Spec.ClassRef)
	}
	if got.Spec.Repo != want.Spec.Repo {
		t.Errorf("Repo = %q, want %q", got.Spec.Repo, want.Spec.Repo)
	}
	if got.Spec.Task.Prompt != want.Spec.Task.Prompt {
		t.Errorf("Task.Prompt = %q, want %q", got.Spec.Task.Prompt, want.Spec.Task.Prompt)
	}
	if got.Spec.Task.IssueRef == nil || *got.Spec.Task.IssueRef != *want.Spec.Task.IssueRef {
		t.Errorf("Task.IssueRef = %v, want %v", got.Spec.Task.IssueRef, want.Spec.Task.IssueRef)
	}
	if got.Spec.Priority != want.Spec.Priority {
		t.Errorf("Priority = %d, want %d", got.Spec.Priority, want.Spec.Priority)
	}
	if got.Spec.Suspend != want.Spec.Suspend {
		t.Errorf("Suspend = %v, want %v", got.Spec.Suspend, want.Spec.Suspend)
	}

	// Critical regression check: without +kubebuilder:default={} on the
	// Status field itself, status.phase's own default silently does not
	// apply because the API server won't descend into an absent status
	// object.
	if got.Status.Phase != sandboxv1alpha1.PhasePending {
		t.Errorf("Status.Phase = %q, want %q", got.Status.Phase, sandboxv1alpha1.PhasePending)
	}
}

func TestEnvTaskCEL(t *testing.T) {
	t.Run("empty-task-rejected", func(t *testing.T) {
		obj := validEnv("task-empty")
		obj.Spec.Task = sandboxv1alpha1.TaskSpec{}
		err := k8s.Create(ctx, obj)
		if err == nil {
			t.Fatalf("Create succeeded, want rejection")
		}
		if !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) {
			t.Errorf("error is neither Invalid nor BadRequest: %v", err)
		}
		t.Logf("server message: %v", err)
	})

	t.Run("issueref-only-accepted", func(t *testing.T) {
		obj := validEnv("task-issueref-only")
		obj.Spec.Task = sandboxv1alpha1.TaskSpec{
			IssueRef: &sandboxv1alpha1.IssueRef{Repo: "psenna/ai-sandbox", Number: 17},
		}
		if err := k8s.Create(ctx, obj); err != nil {
			t.Errorf("Create failed, want success: %v", err)
		}
	})

	t.Run("prompt-only-accepted", func(t *testing.T) {
		obj := validEnv("task-prompt-only")
		obj.Spec.Task = sandboxv1alpha1.TaskSpec{Prompt: "do the thing"}
		if err := k8s.Create(ctx, obj); err != nil {
			t.Errorf("Create failed, want success: %v", err)
		}
	})

	t.Run("explicit-empty-prompt-no-issueref-rejected", func(t *testing.T) {
		// A typed client with `json:"prompt,omitempty"` can never send an
		// explicit empty string -- Go's encoding/json drops it, which would
		// only exercise the has() half of the CEL rule. Use an unstructured
		// object instead so "prompt": "" is genuinely present in the
		// request body, exercising the size(self.prompt) > 0 half.
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group: sandboxv1alpha1.GroupVersion.Group, Version: sandboxv1alpha1.GroupVersion.Version, Kind: "SandboxEnvironment",
		})
		obj.SetName("task-explicit-empty-prompt")
		obj.SetNamespace(testNamespace)
		obj.Object["spec"] = map[string]interface{}{
			"classRef": map[string]interface{}{"name": "some-class"},
			"repo":     "psenna/ai-sandbox",
			"task": map[string]interface{}{
				"prompt": "",
			},
		}

		err := k8s.Create(ctx, obj)
		if err == nil {
			t.Fatalf("Create succeeded, want rejection")
		}
		if !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) {
			t.Errorf("error is neither Invalid nor BadRequest: %v", err)
		}
		t.Logf("server message: %v", err)
	})
}

func TestEnvImmutability(t *testing.T) {
	base := validEnv("immutable-env")
	if err := k8s.Create(ctx, base); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fresh := func(t *testing.T) *sandboxv1alpha1.SandboxEnvironment {
		t.Helper()
		obj := &sandboxv1alpha1.SandboxEnvironment{}
		if err := k8s.Get(ctx, types.NamespacedName{Name: base.Name, Namespace: base.Namespace}, obj); err != nil {
			t.Fatalf("Get: %v", err)
		}
		return obj
	}

	t.Run("repo-immutable", func(t *testing.T) {
		obj := fresh(t)
		obj.Spec.Repo = "psenna/other-repo"
		err := k8s.Update(ctx, obj)
		if !apierrors.IsInvalid(err) {
			t.Errorf("Update err = %v, want IsInvalid", err)
		}
	})

	t.Run("classref-immutable", func(t *testing.T) {
		obj := fresh(t)
		obj.Spec.ClassRef.Name = "other-class"
		err := k8s.Update(ctx, obj)
		if !apierrors.IsInvalid(err) {
			t.Errorf("Update err = %v, want IsInvalid", err)
		}
	})

	t.Run("task-prompt-immutable", func(t *testing.T) {
		obj := fresh(t)
		obj.Spec.Task.Prompt = "a different prompt"
		err := k8s.Update(ctx, obj)
		if !apierrors.IsInvalid(err) {
			t.Errorf("Update err = %v, want IsInvalid", err)
		}
	})

	t.Run("task-issueref-number-immutable", func(t *testing.T) {
		obj := fresh(t)
		obj.Spec.Task.IssueRef.Number = 99
		err := k8s.Update(ctx, obj)
		if !apierrors.IsInvalid(err) {
			t.Errorf("Update err = %v, want IsInvalid", err)
		}
	})

	t.Run("priority-and-suspend-mutable", func(t *testing.T) {
		obj := fresh(t)
		obj.Spec.Priority = -5
		obj.Spec.Suspend = false
		if err := k8s.Update(ctx, obj); err != nil {
			t.Errorf("Update failed, want success: %v", err)
		}
	})
}

// assertSnapshotStatus checks status.snapshot and status.snapshotAttempt
// against the fixed values TestEnvStatusSubresource wrote. Factored out to
// keep that test's own cyclomatic complexity under gocyclo's threshold.
func assertSnapshotStatus(t *testing.T, status sandboxv1alpha1.SandboxEnvironmentStatus, wantSHA256 string) {
	t.Helper()
	if status.Snapshot == nil || status.Snapshot.Seq != 3 || status.Snapshot.SHA256 != wantSHA256 ||
		status.Snapshot.SizeBytes != 123456 || status.Snapshot.TakenAt == nil || status.Snapshot.DurationMillis != 4200 {
		t.Errorf("Status.Snapshot = %+v", status.Snapshot)
	}
	if status.SnapshotAttempt == nil || status.SnapshotAttempt.Seq != 3 ||
		status.SnapshotAttempt.Phase != sandboxv1alpha1.SnapshotAttemptInProgress || status.SnapshotAttempt.Attempts != 2 {
		t.Errorf("Status.SnapshotAttempt = %+v", status.SnapshotAttempt)
	}
}

func TestEnvStatusSubresource(t *testing.T) {
	obj := validEnv("status-env")
	if err := k8s.Create(ctx, obj); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A change to .status sent through the main resource endpoint must be
	// silently dropped: spec and status are separate subresources.
	obj.Status.Phase = sandboxv1alpha1.PhaseRunning
	if err := k8s.Update(ctx, obj); err != nil {
		t.Fatalf("Update (spec-only): %v", err)
	}

	reGet := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}, reGet); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reGet.Status.Phase != sandboxv1alpha1.PhasePending {
		t.Errorf("Status.Phase after main-endpoint update = %q, want unchanged %q", reGet.Status.Phase, sandboxv1alpha1.PhasePending)
	}

	grantedAt := metav1.Now()
	takenAt := metav1.Now()
	sha256 := strings.Repeat("a1b2c3d4", 8) // 64 lowercase-hex chars

	reGet.Status = sandboxv1alpha1.SandboxEnvironmentStatus{
		Phase: sandboxv1alpha1.PhaseWaiting,
		Slot: sandboxv1alpha1.SlotStatus{
			Granted:   true,
			GrantedAt: &grantedAt,
			LeaseName: "slot-lease-1",
		},
		WaitFor: &sandboxv1alpha1.WaitForStatus{
			Type:   "NotBefore",
			Reason: "waiting on PR #42",
			Params: map[string]string{"pr": "42"},
		},
		Snapshot: &sandboxv1alpha1.SnapshotStatus{
			Seq:            3,
			URI:            "s3://sandbox-snapshots/prod/status-env/3",
			SizeBytes:      123456,
			SHA256:         sha256,
			TakenAt:        &takenAt,
			DurationMillis: 4200,
		},
		SnapshotAttempt: &sandboxv1alpha1.SnapshotAttemptStatus{
			Seq:      3,
			Phase:    sandboxv1alpha1.SnapshotAttemptInProgress,
			Attempts: 2,
			Reason:   "BackendUnreachable",
			Message:  "retrying after a transient S3 5xx",
		},
		FreezeCount:        2,
		WakeCount:          1,
		ArchiveURI:         "s3://sandbox-snapshots/prod/status-env/archive.tar.zst",
		ObservedGeneration: 1,
	}
	apimeta.SetStatusCondition(&reGet.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "SlotGranted",
		Message: "slot granted and sandbox restored",
	})

	if err := k8s.Status().Update(ctx, reGet); err != nil {
		t.Fatalf("Status().Update: %v", err)
	}

	final := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}, final); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if final.Status.Phase != sandboxv1alpha1.PhaseWaiting {
		t.Errorf("Status.Phase = %q, want %q", final.Status.Phase, sandboxv1alpha1.PhaseWaiting)
	}
	if !final.Status.Slot.Granted || final.Status.Slot.GrantedAt == nil || final.Status.Slot.LeaseName != "slot-lease-1" {
		t.Errorf("Status.Slot = %+v", final.Status.Slot)
	}
	if final.Status.WaitFor == nil || final.Status.WaitFor.Type != "NotBefore" || final.Status.WaitFor.Params["pr"] != "42" {
		t.Errorf("Status.WaitFor = %+v", final.Status.WaitFor)
	}
	assertSnapshotStatus(t, final.Status, sha256)
	if final.Status.FreezeCount != 2 || final.Status.WakeCount != 1 {
		t.Errorf("FreezeCount/WakeCount = %d/%d, want 2/1", final.Status.FreezeCount, final.Status.WakeCount)
	}
	if final.Status.ArchiveURI == "" || final.Status.ObservedGeneration != 1 {
		t.Errorf("ArchiveURI/ObservedGeneration = %q/%d", final.Status.ArchiveURI, final.Status.ObservedGeneration)
	}
	if cond := apimeta.FindStatusCondition(final.Status.Conditions, "Ready"); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready condition = %+v", cond)
	}

	t.Run("rejections", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*sandboxv1alpha1.SandboxEnvironmentStatus)
		}{
			{
				name: "bad-phase-enum",
				mutate: func(s *sandboxv1alpha1.SandboxEnvironmentStatus) {
					s.Phase = "Sleeping"
				},
			},
			{
				name: "bad-waitfor-type-enum",
				mutate: func(s *sandboxv1alpha1.SandboxEnvironmentStatus) {
					s.WaitFor = &sandboxv1alpha1.WaitForStatus{Type: "SolarEclipse"}
				},
			},
			{
				name: "bad-snapshot-sha256",
				mutate: func(s *sandboxv1alpha1.SandboxEnvironmentStatus) {
					s.Snapshot = &sandboxv1alpha1.SnapshotStatus{Seq: 1, SHA256: "not-hex"}
				},
			},
			{
				name: "bad-snapshot-attempt-phase-enum",
				mutate: func(s *sandboxv1alpha1.SandboxEnvironmentStatus) {
					s.SnapshotAttempt = &sandboxv1alpha1.SnapshotAttemptStatus{Seq: 1, Phase: "Retrying"}
				},
			},
			{
				name: "snapshot-attempt-message-too-long",
				mutate: func(s *sandboxv1alpha1.SandboxEnvironmentStatus) {
					s.SnapshotAttempt = &sandboxv1alpha1.SnapshotAttemptStatus{
						Seq: 1, Phase: sandboxv1alpha1.SnapshotAttemptFailed,
						Message: strings.Repeat("x", 513),
					}
				},
			},
			{
				name: "snapshot-attempt-negative-seq",
				mutate: func(s *sandboxv1alpha1.SandboxEnvironmentStatus) {
					s.SnapshotAttempt = &sandboxv1alpha1.SnapshotAttemptStatus{Seq: -1, Phase: sandboxv1alpha1.SnapshotAttemptInProgress}
				},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				current := &sandboxv1alpha1.SandboxEnvironment{}
				if err := k8s.Get(ctx, types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}, current); err != nil {
					t.Fatalf("Get: %v", err)
				}
				tc.mutate(&current.Status)
				err := k8s.Status().Update(ctx, current)
				if err == nil {
					t.Fatalf("Status().Update succeeded, want rejection")
				}
				if !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) {
					t.Errorf("error is neither Invalid nor BadRequest: %v", err)
				}
				t.Logf("server message: %v", err)
			})
		}
	})
}
