// Package crdref renders a markdown CRD reference document from
// apiextensionsv1.CustomResourceDefinition values already loaded from the
// committed config/crd/bases/*.yaml (controller-gen's own generated
// output).
//
// It is a pure, deterministic markdown renderer, with the same purity
// contract internal/render/doc.go states for the pod/child-object
// renderer: Render must never import sigs.k8s.io/controller-runtime, never
// touch a live cluster, a clock, or any source of randomness. Two renders
// of the same input CRDs must be byte-for-byte identical -- that is what
// lets `make crd-docs-check` detect drift with a plain diff instead of a
// fuzzy comparison, and it is enforced here by TestRender_Deterministic
// the same way internal/render/determinism_test.go enforces it there.
package crdref
