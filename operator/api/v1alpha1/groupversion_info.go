// Package v1alpha1 contains the sandbox.psenna.dev/v1alpha1 API types.
// +kubebuilder:object:generate=true
// +groupName=sandbox.psenna.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group version used to register these types.
	GroupVersion = schema.GroupVersion{Group: "sandbox.psenna.dev", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
