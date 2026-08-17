package render

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"
)

// defaultWorkspaceSize mirrors the CRD default on
// SandboxClassSpec.Storage.Workspace.Size, so a class object that predates
// CRD defaulting (e.g. built directly in a test) still renders correctly.
const defaultWorkspaceSize = "20Gi"

// renderPVC renders the workspace PersistentVolumeClaim, sized from
// class.Spec.Storage.Workspace and retained across freeze/wake as the warm
// cache (it is never deleted by this operator outside of CR deletion).
func renderPVC(in Inputs) (*acorev1.PersistentVolumeClaimApplyConfiguration, error) {
	names := ChildNames(in.Env.Name)

	size := in.Class.Spec.Storage.Workspace.Size
	if size == "" {
		size = defaultWorkspaceSize
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("parsing storage.workspace.size %q: %w", size, err)
	}

	pvc := acorev1.PersistentVolumeClaim(names.PVC, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithSpec(acorev1.PersistentVolumeClaimSpec().
			WithAccessModes(corev1.ReadWriteOnce).
			// volumeMode is set explicitly (Filesystem, the only mode a
			// workspace mount uses) even though it also happens to be the
			// API server's default: if left unset here, every apply
			// alternates between "field absent, so SSA prunes the
			// previously-owned default value" and "API server re-defaults
			// it", producing a real spec diff -- and therefore a write, a
			// bumped resourceVersion -- on every single reconcile even
			// with nothing to change. Confirmed empirically against
			// envtest while implementing #19; setting it explicitly makes
			// our own apply-configuration match what the server persists,
			// so repeated applies are true no-ops.
			WithVolumeMode(corev1.PersistentVolumeFilesystem).
			WithResources(acorev1.VolumeResourceRequirements().
				WithRequests(corev1.ResourceList{corev1.ResourceStorage: qty})))

	// storageClassName: nil means "use the cluster default" (field absent);
	// an explicit empty string means "no StorageClass" (field present and
	// empty) -- these are meaningfully different, preserve the distinction.
	if scn := in.Class.Spec.Storage.Workspace.StorageClassName; scn != nil {
		pvc.Spec.WithStorageClassName(*scn)
	}

	return pvc, nil
}
