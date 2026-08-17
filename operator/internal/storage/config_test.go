package storage

import (
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestFromS3Backend_ForcePathStyleDefault(t *testing.T) {
	// nil ForcePathStyle means "class object predates CRD defaulting" --
	// must fall back to the CRD's own documented default of true, the same
	// way internal/render/pvc.go falls back to its own CRD-defaulted
	// workspace size.
	cfg, err := FromS3Backend(&v1alpha1.S3Backend{Endpoint: "http://minio:9000", Bucket: "b"})
	if err != nil {
		t.Fatalf("FromS3Backend: %v", err)
	}
	if !cfg.ForcePathStyle {
		t.Error("nil ForcePathStyle should default to true")
	}

	falseVal := false
	cfg2, err := FromS3Backend(&v1alpha1.S3Backend{Endpoint: "http://minio:9000", Bucket: "b", ForcePathStyle: &falseVal})
	if err != nil {
		t.Fatalf("FromS3Backend: %v", err)
	}
	if cfg2.ForcePathStyle {
		t.Error("explicit false ForcePathStyle must be preserved, not overridden to true")
	}

	trueVal := true
	cfg3, err := FromS3Backend(&v1alpha1.S3Backend{Endpoint: "http://minio:9000", Bucket: "b", ForcePathStyle: &trueVal})
	if err != nil {
		t.Fatalf("FromS3Backend: %v", err)
	}
	if !cfg3.ForcePathStyle {
		t.Error("explicit true ForcePathStyle must be preserved")
	}
}

func TestFromS3Backend_NilRejected(t *testing.T) {
	if _, err := FromS3Backend(nil); !IsInvalid(err) {
		t.Errorf("FromS3Backend(nil): err = %v, want ErrInvalid", err)
	}
}

func TestFromS3Backend_FieldMapping(t *testing.T) {
	cfg, err := FromS3Backend(&v1alpha1.S3Backend{
		Endpoint: "https://s3.example.com",
		Bucket:   "my-bucket",
		Region:   "us-west-2",
	})
	if err != nil {
		t.Fatalf("FromS3Backend: %v", err)
	}
	if cfg.Endpoint != "https://s3.example.com" || cfg.Bucket != "my-bucket" || cfg.Region != "us-west-2" {
		t.Errorf("FromS3Backend field mapping mismatch: %+v", cfg)
	}
}

func TestFromPVCBackend(t *testing.T) {
	cfg, err := FromPVCBackend(&v1alpha1.PVCBackend{ClaimName: "claim-a", SubPath: "sub"}, "/mnt/workspace")
	if err != nil {
		t.Fatalf("FromPVCBackend: %v", err)
	}
	if cfg.Root != "/mnt/workspace" {
		t.Errorf("Root = %q, want /mnt/workspace", cfg.Root)
	}

	if _, err := FromPVCBackend(nil, "/mnt"); !IsInvalid(err) {
		t.Errorf("FromPVCBackend(nil, ..): err = %v, want ErrInvalid", err)
	}
	if _, err := FromPVCBackend(&v1alpha1.PVCBackend{ClaimName: "c"}, ""); !IsInvalid(err) {
		t.Errorf("FromPVCBackend(.., \"\"): err = %v, want ErrInvalid", err)
	}
}

func TestLayoutPrefix(t *testing.T) {
	s3Spec := v1alpha1.BackendSpec{Type: v1alpha1.StorageBackendTypeS3, S3: &v1alpha1.S3Backend{Prefix: "s3-prefix"}}
	if got := LayoutPrefix(s3Spec); got != "s3-prefix" {
		t.Errorf("LayoutPrefix(s3) = %q, want s3-prefix", got)
	}

	pvcSpec := v1alpha1.BackendSpec{Type: v1alpha1.StorageBackendTypePVC, PVC: &v1alpha1.PVCBackend{SubPath: "pvc-sub"}}
	if got := LayoutPrefix(pvcSpec); got != "pvc-sub" {
		t.Errorf("LayoutPrefix(pvc) = %q, want pvc-sub", got)
	}

	empty := v1alpha1.BackendSpec{Type: v1alpha1.StorageBackendTypeS3}
	if got := LayoutPrefix(empty); got != "" {
		t.Errorf("LayoutPrefix(s3 with nil S3) = %q, want empty", got)
	}
}
