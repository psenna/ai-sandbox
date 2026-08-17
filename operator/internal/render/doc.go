// Package render is a pure, deterministic renderer for the Kubernetes
// objects a SandboxEnvironment needs besides its pod: the workspace PVC, the
// sidecar's ServiceAccount/Role/RoleBinding, the agent's environment Secret,
// and the entrypoint ConfigMap.
//
// This package MUST NOT import sigs.k8s.io/controller-runtime or any
// client.Client-related package, and must never talk to a cluster, a clock,
// or a source of randomness. Render is a pure function of its Inputs:
// calling it twice with identical Inputs must produce byte-identical
// output (see determinism_test.go). Every consumer of a rendered object
// (internal/controller) is responsible for applying it to a cluster; this
// package only builds the declarative apply-configuration values.
package render
