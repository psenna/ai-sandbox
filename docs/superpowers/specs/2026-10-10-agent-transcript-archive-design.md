# Design: archive agent transcripts at delete time

**Status:** design, not yet implemented.

**Branch:** `feat/agent-transcript-archive`, branched from `main`.

## Why

docker-operator manages coding-agent containers (Claude Code or opencode),
each of which produces a real record of its own work: Claude Code writes a
structured per-session JSONL transcript (tool calls, reasoning, turns);
opencode's equivalent session state is an opaque `opencode.db`, but every
agent regardless of harness already has a continuous raw terminal-output
capture (`tmux pipe-pane` → `.agent-output.log`) via `internal/wsbridge`'s
existing mechanism.

None of this survives `Delete`. `internal/agent/delete.go`'s teardown
removes the `WorkspaceVolume`, `ClaudeConfigVolume` and `DindCacheVolume` as
its last step, with no copy-out first — confirmed directly against the
current code, not assumed. The one thing that *does* survive deletion today
(the centralized file store's `agents/<id>/` subtree) is populated only by
whatever the agent itself wrote into its own store mount; nothing copies an
agent's session history into it.

The user wants to start keeping that history — specifically so it can be
read later (by a human, and eventually maybe by an automated pass) to
notice real patterns in how agents work and use that to improve this
project's own CLAUDE.md and skills. This is architectural: a new capture
mechanism, a new area in an existing subsystem, and a new step in an
existing lifecycle path, not a tweak to a single file.

## Decisions locked during brainstorming

1. **Storage first, analysis later.** This design covers durable, browsable
   storage only. No automated mining/summarization is built now — but
   nothing about this design prevents adding that later: a transcript is
   just a file sitting in the store, readable by any future tool.
2. **Both harnesses, different sources, side by side.** Claude Code agents
   get their real JSONL session transcript (the richer signal). opencode
   agents get `.agent-output.log` (harness-agnostic, already captured,
   already durable for the agent's lifetime) as their best available
   substitute — no reader for `opencode.db` is built; it's a binary/SQLite
   format nobody has asked to mine, and building a parser for it now would
   be premature.
3. **Capture trigger: `Delete` only.** No periodic/incremental capture
   while an agent is alive. Simpler, one trigger point, and it's the one
   natural hook this codebase already demonstrates reaching from
   `internal/agent` into `internal/filestore` (`PurgeAgentFiles`, called
   from the same delete-adjacent surface). Accepted cost: a hard crash or a
   container removed outside the operator's own `Delete` path loses that
   agent's transcript — acceptable for a learning-corpus feature, not
   acceptable for anything safety-critical (this isn't).
4. **A new, separate top-level store area: `transcripts/<id>/`.** NOT
   inside `agents/<id>/`. `agents/<id>/` already means "files the agent
   itself wrote," and `DELETE /api/agents/{id}?purge_files=true` wipes that
   whole subtree. If transcripts lived there, "purge this agent's files"
   and "destroy the very history you're trying to preserve" would be the
   same action. A separate area means `purge_files` never touches a
   transcript, and a transcript needs its own explicit action later if one
   is ever deliberately removed.
5. **Self-describing via `metadata.json`.** Once an agent is deleted, its
   bare ID tells you nothing. Each `transcripts/<id>/` gets a small
   `metadata.json` (name, description, harness, backend, repo, image,
   created/deleted timestamps) captured from the live `store.Agent` record
   at the moment of deletion, so the archive means something months later
   with no other record of that agent left anywhere.
6. **No new UI.** Verified directly: the existing Files browser
   (`internal/filestore`'s `List`, `internal/api/handlers.go`'s
   `/api/files*` routes, `web/files.js`) is fully generic over whatever
   top-level directories exist under the store root — `AgentsDir`/`SharedDir`
   are just two named constants for two *specific* existing concepts, not a
   restriction on what else can exist. `transcripts/<id>/` shows up in the
   existing browser with zero code changes there.
7. **Best-effort, never blocks `Delete`.** If archiving fails for any
   reason (unreadable volume, filestore briefly unavailable, whatever), log
   a warning and let the agent's own explicit deletion proceed anyway. This
   matches the existing pattern for every other delete-adjacent cleanup
   step in this codebase (`PurgeAgentFiles` and the rest of `teardown` are
   all best-effort); a learning archive is valuable, not safety-critical,
   and must never hold a user's deletion hostage.
8. **No secret redaction in v1.** A transcript is whatever the agent
   actually saw and typed, which could include tokens it had access to.
   Explicitly out of scope for now — redaction is real design work (what
   counts as a secret, false positive/negative trade-offs) that can be
   added later if it turns out to matter. Documented here as a known,
   accepted risk, not an oversight: this is a straight continuation of this
   project's existing trust model (agents already hold low-value tokens;
   the file store itself has no existing secret-scanning anywhere), not a
   new risk class this feature introduces on its own.
9. **If the file store is disabled (`FILESTORE_DIR=""`), archiving is
   skipped.** Same governance `FilestoreEnabled()` already provides for
   every other file-store-dependent feature — not a new failure mode to
   handle specially.

## What gets captured, and how

**Extraction mechanism.** Every existing in-container read in this
codebase (`wsbridge.ReadOutput`, `ReadActivity`, the terminal bridge) is
`docker exec`-based, which requires the container to be *running*. But
`Delete` is valid from `StatusRunning`, `StatusStopped`, or `StatusError`
(confirmed: `updatableStatus`'s sibling check in `delete.go` is more
permissive than `Update`'s), so archiving cannot assume an execable
process. Docker's copy-from-container API (the mechanism behind `docker cp`)
reads a tar stream directly off the container's filesystem via the daemon,
with no running process required, and works identically whether the
container is up or stopped. The underlying client library docker-operator
already vendors (`github.com/moby/moby/client`) ships this
(`container_copy.go`'s `CopyFromContainer`); `internal/dockerclient` has no
wrapper for it yet — one thin wrapper, same shape as `VolumeUsage`/
`ContainerStats`, is all that's needed:

```go
// internal/dockerclient/client.go (new method)
func (d *Docker) CopyFromContainer(ctx context.Context, containerID, path string) (io.ReadCloser, error)
```

**Per harness:**

- **Claude Code:** copy the whole `$CLAUDE_CONFIG_DIR/projects/` directory
  wholesale — NOT just the one subdirectory for the agent's own cwd.
  Claude Code encodes the working directory into that subdirectory's name
  by its own internal convention; replicating that encoding here would be
  one more thing to keep in sync with an implementation detail this
  project doesn't own. Since every agent's `ClaudeConfigVolume` is its own
  separate volume, there is no cross-agent collision risk in copying
  everything under `projects/` — in practice it will contain exactly one
  subdirectory (every agent's `WORKDIR` is `/workspace`), but copying the
  whole thing wholesale is simpler and stays correct even if that ever
  changes. This also naturally captures every session `.jsonl` file across
  the agent's life, not just the newest one — the goal is learning from the
  full history. This path is inside `a.ClaudeConfigVolume`, mounted at
  `configMount` (`internal/agent/create.go`'s `configMount`/`applyClaudeEnv`).
- **opencode:** copy `.agent-output.log` from the `WorkspaceVolume`
  (`internal/wsbridge/output.go`'s `OutputLogPath`, already written
  continuously by `tmux pipe-pane` per `agent/tmux-boot.sh`).

**Destination, via `internal/filestore`'s existing, already path-generic
`Save`/`Mkdir`** (confirmed: `AgentsDir`/`SharedDir` are two named
constants for two specific concepts, not a hard-coded allow-list; `List`,
and the `/api/files*` handlers that call it, take an arbitrary relative
path):

```
transcripts/<agent-id>/
  metadata.json
  projects/...            -- Claude Code only: the whole copied directory, structure preserved
  output.log              -- opencode only
```

**`metadata.json`** is built from the live `store.Agent` record at the
start of `Delete`, before anything is torn down:

```json
{
  "id": "agt_...",
  "name": "...",
  "description": "...",
  "harness": "claude-code",
  "backend": "anthropic",
  "repo": "owner/repo",
  "image": "...",
  "created_at": "...",
  "deleted_at": "..."
}
```

## Where this fits in `Delete`

`internal/agent/delete.go`'s `teardown` gains a new first step, before the
container, dinernet, or volume removal currently there:

```go
func (m *Manager) teardown(ctx context.Context, a store.Agent) error {
	m.archiveTranscript(ctx, a) // best-effort; logs and continues on any error
	// ... existing steps, unchanged ...
}
```

`archiveTranscript` never returns an error to its caller — any failure
(container gone, `CopyFromContainer` erroring, filestore write failing) is
logged at Warn and swallowed, exactly like this function's sibling
best-effort steps.

## Interaction with `purge_files=true`

None, by construction. `PurgeAgentFiles` (called separately, only when the
caller opts in) only ever reaches `agents/<id>/`. `transcripts/<id>/` is a
different top-level area; nothing in this design or in `PurgeAgentFiles`
touches it. Deleting an agent with `purge_files=true` still archives its
transcript first — "purge this agent's own files" and "forget this agent's
history" are two different actions a user can take independently, and
decision #4 above is exactly why they're kept separate.

## Testing

- `internal/dockerclient`: new `CopyFromContainer` method + a conformance
  test (fake + real-daemon legs, this package's established pattern) --
  covering a running container, a stopped container, and a missing path.
- `internal/agent`: `archiveTranscript`'s harness branch (Claude Code vs.
  opencode path selection), `metadata.json`'s exact field set, and that a
  `CopyFromContainer`/filestore-write failure is logged but does not
  propagate — `Delete` still succeeds and still removes every volume.
- `internal/filestore`: no new methods needed (confirmed path-generic), so
  no new tests needed there beyond what already covers `Save`/`Mkdir`.
- No new web/UI tests: the existing Files browser needs no code change, so
  nothing new to test there either — a human confirming `transcripts/<id>/`
  is actually visible and downloadable through the existing Files UI after
  a real delete is the only verification this needs in the browser.
