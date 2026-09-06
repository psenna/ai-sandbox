// Package filestore is a thin, hardened view over one root directory: the
// shared file-store volume as the docker-operator process sees it
// (config.Config.FilestoreDir). It is the operator-side half of the
// centralized per-agent file store (issue #122); the daemon-side half is the
// per-agent subpath mount internal/agent adds to every agent container
// (config.Config.FilestoreVolume, mounted at /workspace/store).
//
// Those two -- FilestoreDir and FilestoreVolume -- are TWO NAMES FOR THE SAME
// STORAGE. The operator pre-creates agents/<id>/ under FilestoreDir with
// EnsureAgentDir before an agent is created; the daemon then resolves the
// "agents/<id>" subpath against FilestoreVolume when it mounts it into the
// container. If the two names do not point at the same volume, the mismatch
// surfaces at container-create time as "subpath not found".
//
// Agents never reach this package over the network: the file API
// (internal/api's /api/files* routes) is the operator's, behind the same
// operator Bearer as the rest of /api, and an agent only ever sees its own
// subtree through the mount. Per-agent isolation is enforced by Docker (the
// subpath mount), not by this package.
//
// Layout under the root: agents/<id>/ per agent, and nothing else in v1 (no
// shared area). The store deliberately OUTLIVES agent teardown -- deleting an
// agent (internal/agent.Delete, create-failure rollback, the reconcile pass)
// leaves agents/<id>/ untouched; only DELETE /api/agents/{id}?purge_files=true
// or an explicit delete through the web UI removes it.
//
// Every filesystem operation goes through os.Root (Go 1.24+), so a symlink an
// agent plants inside its own subtree cannot be used to escape the root, and
// there is no TOCTOU window between validating a path and opening it. A
// strict segment validator (cleanRel) runs first anyway, for good error
// messages and to reject "..", absolute paths and control characters before
// any syscall.
package filestore
