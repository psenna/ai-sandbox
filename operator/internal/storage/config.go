package storage

import (
	"fmt"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// FromS3Backend adapts a *v1alpha1.S3Backend into an S3Config. Does NOT
// resolve credentials -- see doc.go's credential-resolution-boundary
// section; the caller resolves CredentialsSecretRef into a Credentials
// value separately and passes it to NewS3.
func FromS3Backend(b *v1alpha1.S3Backend) (S3Config, error) {
	if b == nil {
		return S3Config{}, fmt.Errorf("%w: s3 backend spec is required", ErrInvalid)
	}
	cfg := S3Config{
		Endpoint: b.Endpoint,
		Bucket:   b.Bucket,
		Region:   b.Region,
	}
	// ForcePathStyle is a *bool with a CRD default of true (see
	// api/v1alpha1/sandboxclass_types.go's +kubebuilder:default=true); a
	// nil pointer here means "a class object built before CRD defaulting
	// ran" (e.g. directly in a test), so treat nil the same way
	// internal/render/pvc.go treats its own CRD-defaulted fields: fall back
	// to the documented default rather than silently taking Go's own
	// zero value (false), which would flip addressing style.
	if b.ForcePathStyle == nil {
		cfg.ForcePathStyle = true
	} else {
		cfg.ForcePathStyle = *b.ForcePathStyle
	}
	return cfg, nil
}

// FromPVCBackend adapts a *v1alpha1.PVCBackend into an FSConfig, rooted at
// mountRoot (the path this backend's PVC is mounted at inside the operator
// pod -- wiring that mount is out of scope for this issue, see doc.go's G5
// gap).
func FromPVCBackend(b *v1alpha1.PVCBackend, mountRoot string) (FSConfig, error) {
	if b == nil {
		return FSConfig{}, fmt.Errorf("%w: pvc backend spec is required", ErrInvalid)
	}
	if mountRoot == "" {
		return FSConfig{}, fmt.Errorf("%w: pvc backend mount root is required", ErrInvalid)
	}
	return FSConfig{Root: mountRoot}, nil
}

// LayoutPrefix returns the object-key/path prefix a BackendSpec configures:
// S3Backend.Prefix for an S3 backend, PVCBackend.SubPath for a PVC backend,
// or "" if the relevant sub-spec is unset.
func LayoutPrefix(b v1alpha1.BackendSpec) string {
	switch b.Type {
	case v1alpha1.StorageBackendTypeS3:
		if b.S3 != nil {
			return b.S3.Prefix
		}
	case v1alpha1.StorageBackendTypePVC:
		if b.PVC != nil {
			return b.PVC.SubPath
		}
	}
	return ""
}
