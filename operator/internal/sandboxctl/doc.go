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
// Deciding when a declared probe is SATISFIED is issue #30's job. The
// snapshot itself -- workload-container teardown, tar+zstd, upload via
// internal/storage, manifest.json/latest.json, the resume marker, and the
// status.snapshot(+snapshotAttempt) write -- is issue #28's job, implemented
// in snapshot.go/snapshotconfig.go/snapcreds.go/marker.go/exclusions.go/
// engine.go. This package now DOES import internal/storage (only from those
// #28 files): building an S3 storage.Backend from CLI-flag-projected
// configuration and streaming the actual archive/upload is unavoidably this
// package's job, since the sidecar is the only process running inside the
// pod with the workspace and agent-home volumes mounted.
//
// # Wake (#29)
//
// This package also owns the wake path: restore, integrity verification,
// and the warm-cache marker contract. Unlike freeze (which runs inside the
// long-lived sandboxctl sidecar or the recovery Job), restore runs as a
// ONE-SHOT init container -- the `restore` subcommand of this SAME binary
// (runrestore.go, restore.go), ordered LAST among init containers, after
// the sandboxctl native sidecar and any engine init containers (see
// internal/render/pod.go). This is structural, not a style choice: a plain
// init container under restartPolicy: Never is the only way "never start
// the agent on a partially restored workspace" is enforced by the kubelet
// itself -- a non-zero exit fails the whole pod rather than merely
// reporting an unhealthy container. warmmarker.go/wakemarker.go/purge.go/
// restoreconfig.go round out the implementation; retrystep.go's retryStep
// is shared, unmodified in behavior, between the freeze and restore paths.
package sandboxctl
