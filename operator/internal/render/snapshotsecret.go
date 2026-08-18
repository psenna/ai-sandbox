package render

import (
	corev1 "k8s.io/api/core/v1"
	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// Data keys for the rendered snapshot-credentials Secret. These MUST equal
// internal/storage's exported constants SecretKeyAccessKeyID/
// SecretKeySecretAccessKey/SecretKeySessionToken -- snapshotsecret_test.go
// asserts this equality (importing internal/storage FROM THE TEST ONLY),
// keeping this package's production import graph free of internal/storage
// (and therefore the AWS SDK): internal/render must stay pure, with zero
// controller-runtime and zero internal/storage imports (enforced by CI's
// grep check).
const (
	snapshotSecretKeyAccessKeyID     = "accessKeyID"
	snapshotSecretKeySecretAccessKey = "secretAccessKey"
	snapshotSecretKeySessionToken    = "sessionToken"
)

// renderSnapshotSecret renders the per-environment Secret carrying the S3
// snapshot credentials the sandboxctl sidecar needs to take a freeze
// snapshot (#28). Resolves internal/storage/doc.go's gap G1 (the sidecar
// has no RBAC to read the operator-namespace Secret directly -- see
// internal/render/rbac.go's renderRole doc comment) by having the OPERATOR
// resolve the class-referenced credentials Secret and project ONLY the
// needed values into a new Secret it owns, mounted into the sandboxctl
// container ONLY (see pod.go's sidecarContainer).
//
// Returns nil unless the class's storage backend is S3: PVC and any future
// backend need no such projection.
func renderSnapshotSecret(in Inputs) *acorev1.SecretApplyConfiguration {
	b := in.Class.Spec.Storage.Backend
	if b.Type != v1alpha1.StorageBackendTypeS3 || b.S3 == nil {
		return nil
	}

	names := ChildNames(in.Env.Name)
	data := map[string][]byte{
		snapshotSecretKeyAccessKeyID:     []byte(in.Credentials.SnapshotAccessKeyID),
		snapshotSecretKeySecretAccessKey: []byte(in.Credentials.SnapshotSecretAccessKey),
	}
	if in.Credentials.SnapshotSessionToken != "" {
		data[snapshotSecretKeySessionToken] = []byte(in.Credentials.SnapshotSessionToken)
	}

	return acorev1.Secret(names.SnapshotSecret, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithType(corev1.SecretTypeOpaque).
		WithData(data)
}
