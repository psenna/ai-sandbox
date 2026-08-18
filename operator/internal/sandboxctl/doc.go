// Package sandboxctl implements the agent-to-operator control channel
// (issue #27): a localhost-only HTTP API that lets the agent container
// declare a wait condition, report a run result, or leave a progress
// breadcrumb, without ever holding a Kubernetes credential itself.
//
// # Where this runs
//
// sandboxctl runs as a standalone process in the environment pod, as a
// native sidecar (KEP-753: an init container with restartPolicy Always),
// NOT inside controller-runtime's manager/cache/informer machinery. It
// builds a single, direct, UNCACHED client.Client from rest.InClusterConfig()
// (see run.go) and talks to the API server only for the two things its Role
// permits: GET its own SandboxEnvironment (poller.go) and PATCH its own
// status subresource (store.go).
//
// It must never construct a controller-runtime manager, cache, or informer:
// the rendered Role (internal/render/rbac.go) grants no list/watch on
// sandboxenvironments, so an informer would fail closed at startup anyway,
// and a manager would pull in leader-election and health-probe machinery
// this single-tenant, single-object process has no use for.
//
// # Security model
//
// The agent container never holds a Kubernetes credential (see
// internal/render/pod.go's RenderPod doc comments: the projected
// ServiceAccount token volume is mounted into the sandboxctl container
// ONLY). Every request from the agent is validated fail-closed against an
// allowlist (probe.go) before anything is patched into the environment's
// status, and the sidecar only ever touches its OWN environment's status
// (enforced by RBAC resourceNames, proven by internal/controller's
// sidecarpatch_test.go and rbac_test.go).
//
// # Scope
//
// This package owns the SCHEMA and the freeze SIGNAL/QUIESCE only:
//   - accept a well-formed wait declaration, validate it, write it honestly
//     to status.waitFor (probe.go, store.go);
//   - accept a run result and write it to status.agentResult (store.go);
//   - detect its own environment entering Freezing (by periodic poll, since
//     the Role grants no watch) and quiesce the API (poller.go, freeze.go).
//
// Deciding when a declared probe is SATISFIED is issue #30's job, and the
// snapshot itself (workload-container teardown, tar+zstd, upload via
// internal/storage, manifest.json/latest.json, the resume marker) is issue
// #28's job in full. This package does not import internal/storage at all.
package sandboxctl
