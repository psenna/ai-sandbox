package render

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// TestSnapshotSecretKeys_MatchStorageConstants is the ONE place in this
// package's test suite that imports internal/storage: it asserts the local
// key-name constants equal storage's exported constants, without letting
// snapshotsecret.go itself (production code) import internal/storage. See
// snapshotsecret.go's doc comment.
func TestSnapshotSecretKeys_MatchStorageConstants(t *testing.T) {
	if snapshotSecretKeyAccessKeyID != storage.SecretKeyAccessKeyID {
		t.Errorf("snapshotSecretKeyAccessKeyID = %q, want %q", snapshotSecretKeyAccessKeyID, storage.SecretKeyAccessKeyID)
	}
	if snapshotSecretKeySecretAccessKey != storage.SecretKeySecretAccessKey {
		t.Errorf("snapshotSecretKeySecretAccessKey = %q, want %q", snapshotSecretKeySecretAccessKey, storage.SecretKeySecretAccessKey)
	}
	if snapshotSecretKeySessionToken != storage.SecretKeySessionToken {
		t.Errorf("snapshotSecretKeySessionToken = %q, want %q", snapshotSecretKeySessionToken, storage.SecretKeySessionToken)
	}
}

func testEnvForSnapshotSecret() *v1alpha1.SandboxEnvironment {
	return &v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "env-a", Namespace: "ns-a", UID: "uid-a"},
		Spec:       v1alpha1.SandboxEnvironmentSpec{ClassRef: v1alpha1.ClassRef{Name: "class-a"}, Repo: "o/r", Task: v1alpha1.TaskSpec{Prompt: "hi"}},
	}
}

func TestRenderSnapshotSecret_NilForPVCBackend(t *testing.T) {
	class := &v1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class-a"},
		Spec: v1alpha1.SandboxClassSpec{
			Agent:   v1alpha1.AgentSpec{Image: "img"},
			Storage: v1alpha1.StorageSpec{Backend: v1alpha1.BackendSpec{Type: v1alpha1.StorageBackendTypePVC, PVC: &v1alpha1.PVCBackend{ClaimName: "c"}}},
		},
	}
	got := renderSnapshotSecret(Inputs{Env: testEnvForSnapshotSecret(), Class: class})
	if got != nil {
		t.Errorf("renderSnapshotSecret() = %v, want nil for a PVC backend", got)
	}
}

func TestRenderSnapshotSecret_S3Backend(t *testing.T) {
	class := &v1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class-a"},
		Spec: v1alpha1.SandboxClassSpec{
			Agent: v1alpha1.AgentSpec{Image: "img"},
			Storage: v1alpha1.StorageSpec{Backend: v1alpha1.BackendSpec{
				Type: v1alpha1.StorageBackendTypeS3,
				S3: &v1alpha1.S3Backend{
					Endpoint: "http://minio:9000", Bucket: "bucket",
					CredentialsSecretRef: v1alpha1.SecretKeyRef{Name: "creds"},
				},
			}},
		},
	}
	creds := Credentials{SnapshotAccessKeyID: "AKID", SnapshotSecretAccessKey: "SECRET"}
	got := renderSnapshotSecret(Inputs{Env: testEnvForSnapshotSecret(), Class: class, Credentials: creds})
	if got == nil {
		t.Fatal("renderSnapshotSecret() = nil, want non-nil for an S3 backend")
	}
	if got.Data == nil {
		t.Fatal("Data is nil")
	}
	if string(got.Data[snapshotSecretKeyAccessKeyID]) != "AKID" {
		t.Errorf("accessKeyID = %q, want AKID", got.Data[snapshotSecretKeyAccessKeyID])
	}
	if string(got.Data[snapshotSecretKeySecretAccessKey]) != "SECRET" {
		t.Errorf("secretAccessKey = %q, want SECRET", got.Data[snapshotSecretKeySecretAccessKey])
	}
	if _, ok := got.Data[snapshotSecretKeySessionToken]; ok {
		t.Error("sessionToken key present despite an empty SnapshotSessionToken")
	}
}
