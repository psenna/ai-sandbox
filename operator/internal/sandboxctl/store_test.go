package sandboxctl

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return scheme
}

func TestPatchStatus_RetriesOnConflictThenSucceedsAndPatchIsFieldScoped(t *testing.T) {
	scheme := testScheme(t)
	env := &v1alpha1.SandboxEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "env-a", Namespace: "ns-a"}}

	attempts := 0
	var lastPatchBody []byte
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(env).
		WithStatusSubresource(&v1alpha1.SandboxEnvironment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				attempts++
				if attempts <= 2 {
					return apierrors.NewConflict(schema.GroupResource{Group: "sandbox.psenna.dev", Resource: "sandboxenvironments"}, "env-a", fmt.Errorf("conflict on attempt %d", attempts))
				}
				data, derr := patch.Data(obj)
				if derr == nil {
					lastPatchBody = data
				}
				return cli.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	store := newEnvStore(c, "ns-a", "env-a")
	now := time.Now()
	err := store.DeclareWait(context.Background(), WaitProbe{
		Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{"duration": "1h"},
	}, now)
	if err != nil {
		t.Fatalf("DeclareWait: %v", err)
	}
	if attempts < 3 {
		t.Fatalf("SubResourcePatch called %d times, want >= 3 (2 conflicts + 1 success)", attempts)
	}

	var got v1alpha1.SandboxEnvironment
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns-a", Name: "env-a"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.WaitFor == nil || got.Status.WaitFor.Type != v1alpha1.WaitTypeNotBefore {
		t.Fatalf("Status.WaitFor = %+v, want it set", got.Status.WaitFor)
	}

	// The field-level restriction proof: the final patch body touches ONLY
	// status.waitFor (plus metadata.resourceVersion for the optimistic
	// lock), never any other field.
	if lastPatchBody == nil {
		t.Fatal("no patch body captured")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(lastPatchBody, &top); err != nil {
		t.Fatalf("unmarshal patch body: %v (body: %s)", err, lastPatchBody)
	}
	for k := range top {
		if k != "status" && k != "metadata" {
			t.Errorf("patch body has unexpected top-level key %q: %s", k, lastPatchBody)
		}
	}
	if statusRaw, ok := top["status"]; ok {
		var status map[string]json.RawMessage
		if err := json.Unmarshal(statusRaw, &status); err != nil {
			t.Fatalf("unmarshal status: %v", err)
		}
		if len(status) != 1 {
			t.Errorf("status patch has %d keys, want exactly 1 (waitFor): %s", len(status), statusRaw)
		}
		if _, ok := status["waitFor"]; !ok {
			t.Errorf("status patch does not contain waitFor: %s", statusRaw)
		}
	} else {
		t.Error("patch body has no status key")
	}
	if metaRaw, ok := top["metadata"]; ok {
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(metaRaw, &meta); err != nil {
			t.Fatalf("unmarshal metadata: %v", err)
		}
		for k := range meta {
			if k != "resourceVersion" {
				t.Errorf("metadata patch has unexpected key %q, want only resourceVersion: %s", k, metaRaw)
			}
		}
	}
}

func TestPatchStatus_ForbiddenIsNotRetried(t *testing.T) {
	scheme := testScheme(t)
	env := &v1alpha1.SandboxEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "env-b", Namespace: "ns-b"}}

	attempts := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(env).
		WithStatusSubresource(&v1alpha1.SandboxEnvironment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				attempts++
				return apierrors.NewForbidden(schema.GroupResource{Group: "sandbox.psenna.dev", Resource: "sandboxenvironments"}, "env-b", fmt.Errorf("denied"))
			},
		}).
		Build()

	store := newEnvStore(c, "ns-b", "env-b")
	err := store.DeclareWait(context.Background(), WaitProbe{
		Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{"duration": "1h"},
	}, time.Now())
	if err == nil {
		t.Fatal("DeclareWait: expected error, got nil")
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("error = %v, want Forbidden", err)
	}
	if attempts != 1 {
		t.Errorf("SubResourcePatch called %d times, want exactly 1 (Forbidden must not be retried)", attempts)
	}
}

func TestReportDone_IdempotentRepeatIssuesNoPatch(t *testing.T) {
	scheme := testScheme(t)
	exitCode := int32(0)
	reportedAt := metav1.NewTime(time.Now().Add(-time.Minute))
	env := &v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "env-c", Namespace: "ns-c"},
		Status: v1alpha1.SandboxEnvironmentStatus{
			AgentResult: &v1alpha1.AgentResultStatus{
				Outcome: v1alpha1.AgentOutcomeSucceeded, Message: "done", ExitCode: &exitCode, ReportedAt: &reportedAt,
			},
		},
	}

	attempts := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(env).
		WithStatusSubresource(&v1alpha1.SandboxEnvironment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				attempts++
				return cli.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	store := newEnvStore(c, "ns-c", "env-c")
	idempotent, err := store.ReportDone(context.Background(), Result{
		Outcome: v1alpha1.AgentOutcomeSucceeded, Message: "done", ExitCode: &exitCode,
	}, time.Now())
	if err != nil {
		t.Fatalf("ReportDone: %v", err)
	}
	if !idempotent {
		t.Error("idempotent = false, want true for an identical repeat")
	}
	if attempts != 0 {
		t.Errorf("SubResourcePatch called %d times, want 0 for a no-op patch", attempts)
	}
}

func TestReportDone_DifferingRepeatErrors(t *testing.T) {
	scheme := testScheme(t)
	env := &v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "env-d", Namespace: "ns-d"},
		Status: v1alpha1.SandboxEnvironmentStatus{
			AgentResult: &v1alpha1.AgentResultStatus{Outcome: v1alpha1.AgentOutcomeSucceeded, Message: "first"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(env).WithStatusSubresource(&v1alpha1.SandboxEnvironment{}).Build()

	store := newEnvStore(c, "ns-d", "env-d")
	_, err := store.ReportDone(context.Background(), Result{Outcome: v1alpha1.AgentOutcomeFailed, Message: "second"}, time.Now())
	if err == nil {
		t.Fatal("expected ErrResultAlreadyReported, got nil")
	}
}

func TestDeclareWait_AlreadyDeclaredErrors(t *testing.T) {
	scheme := testScheme(t)
	declaredAt := metav1.NewTime(time.Now())
	env := &v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "env-e", Namespace: "ns-e"},
		Status: v1alpha1.SandboxEnvironmentStatus{
			WaitFor: &v1alpha1.WaitForStatus{Type: v1alpha1.WaitTypeNotBefore, Reason: "already", DeclaredAt: &declaredAt},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(env).WithStatusSubresource(&v1alpha1.SandboxEnvironment{}).Build()

	store := newEnvStore(c, "ns-e", "env-e")
	err := store.DeclareWait(context.Background(), WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{"duration": "1h"}}, time.Now())
	if err == nil {
		t.Fatal("expected ErrWaitAlreadyDeclared, got nil")
	}
}

func TestStore_RefreshAndSnapshot(t *testing.T) {
	scheme := testScheme(t)
	env := &v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "env-f", Namespace: "ns-f"},
		Status:     v1alpha1.SandboxEnvironmentStatus{Phase: v1alpha1.PhaseRunning, FreezeCount: 2},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(env).WithStatusSubresource(&v1alpha1.SandboxEnvironment{}).Build()

	store := newEnvStore(c, "ns-f", "env-f")
	if snap := store.Snapshot(); snap.Fresh {
		t.Error("Snapshot() before any Refresh should be zero-value (not Fresh)")
	}
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap := store.Snapshot()
	if !snap.Fresh || snap.Phase != v1alpha1.PhaseRunning || snap.FreezeCount != 2 {
		t.Errorf("Snapshot() = %+v, want Fresh Running/FreezeCount=2", snap)
	}
}

func TestRecordSnapshotAttempt_PatchesStatus(t *testing.T) {
	scheme := testScheme(t)
	env := &v1alpha1.SandboxEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "env-g", Namespace: "ns-g"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(env).WithStatusSubresource(&v1alpha1.SandboxEnvironment{}).Build()

	store := newEnvStore(c, "ns-g", "env-g")
	now := time.Now()
	if err := store.RecordSnapshotAttempt(context.Background(), SnapshotAttempt{
		Seq: 3, Phase: v1alpha1.SnapshotAttemptInProgress, Attempts: 1,
	}, now); err != nil {
		t.Fatalf("RecordSnapshotAttempt: %v", err)
	}

	var got v1alpha1.SandboxEnvironment
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns-g", Name: "env-g"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.SnapshotAttempt == nil {
		t.Fatal("Status.SnapshotAttempt is nil")
	}
	if got.Status.SnapshotAttempt.Seq != 3 || got.Status.SnapshotAttempt.Phase != v1alpha1.SnapshotAttemptInProgress || got.Status.SnapshotAttempt.Attempts != 1 {
		t.Errorf("SnapshotAttempt = %+v, want Seq=3 InProgress Attempts=1", got.Status.SnapshotAttempt)
	}
}

func TestRecordSnapshot_SucceedsAndFlipsAttempt(t *testing.T) {
	scheme := testScheme(t)
	env := &v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "env-h", Namespace: "ns-h"},
		Status: v1alpha1.SandboxEnvironmentStatus{
			SnapshotAttempt: &v1alpha1.SnapshotAttemptStatus{Seq: 0, Phase: v1alpha1.SnapshotAttemptInProgress, Attempts: 2},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(env).WithStatusSubresource(&v1alpha1.SandboxEnvironment{}).Build()

	store := newEnvStore(c, "ns-h", "env-h")
	takenAt := time.Now()
	if err := store.RecordSnapshot(context.Background(), SnapshotRecord{
		Seq: 0, URI: "s3://bucket/key", SizeBytes: 1024, SHA256: "abc123", TakenAt: takenAt, DurationMillis: 500,
	}, time.Now()); err != nil {
		t.Fatalf("RecordSnapshot: %v", err)
	}

	var got v1alpha1.SandboxEnvironment
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns-h", Name: "env-h"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Snapshot == nil || got.Status.Snapshot.Seq != 0 || got.Status.Snapshot.SizeBytes != 1024 {
		t.Fatalf("Status.Snapshot = %+v, want Seq=0 SizeBytes=1024", got.Status.Snapshot)
	}
	if got.Status.SnapshotAttempt == nil || got.Status.SnapshotAttempt.Phase != v1alpha1.SnapshotAttemptSucceeded {
		t.Fatalf("Status.SnapshotAttempt = %+v, want Phase=Succeeded", got.Status.SnapshotAttempt)
	}
}

func TestRecordSnapshot_RefusesSeqRegression(t *testing.T) {
	scheme := testScheme(t)
	env := &v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "env-i", Namespace: "ns-i"},
		Status: v1alpha1.SandboxEnvironmentStatus{
			Snapshot: &v1alpha1.SnapshotStatus{Seq: 5},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(env).WithStatusSubresource(&v1alpha1.SandboxEnvironment{}).Build()

	store := newEnvStore(c, "ns-i", "env-i")
	err := store.RecordSnapshot(context.Background(), SnapshotRecord{Seq: 4, URI: "s3://bucket/key"}, time.Now())
	if err == nil {
		t.Fatal("expected ErrSnapshotSeqRegression, got nil")
	}
}
