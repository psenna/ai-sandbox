---
name: unfreeze
description: Use when you are resuming after a freeze — the pod restarted, /workspace/.sandbox/RESUME.md or last-wake.json exists, or a service you started is gone. Wake restored /workspace and your agent home into a fresh pod with none of your containers, processes, or caches.
---

# Unfreeze (what actually happened when you woke up)

You are running in a **new pod**, created from a snapshot the operator took
when you declared a wait (or were suspended). The `freeze` skill told you what
was destroyed at freeze time; this skill tells you what the wake restored,
what it did not, and what to check before you trust anything.

## How to tell you were woken

1. Read `/workspace/.sandbox/RESUME.md` (prose) and
   `/workspace/.sandbox/last-freeze.json` (machine-readable) **first** — they
   describe the freeze that produced you.
2. Read `/workspace/.sandbox/last-wake.json` — the restore container wrote it
   as its last step: which snapshot you came from, whether the workspace was
   restored **warm** (from the retained PVC, nothing downloaded) or **cold**
   (from S3), when, and whether the class or agent image changed since the
   snapshot.
3. `curl $SANDBOX_SIDECAR_URL/v1/status` — if `wakeCount > 0`, this pod is a
   resumption. Your transcript records what you *did*; the markers record what
   *survived*. Where they disagree, the markers win.

## What is physically different

- A fresh pod: new container ID, new hostname, new PID namespace.
- `/tmp` is empty. OS packages you installed are gone.
- **The container image/layer cache is cold.** Your first `docker build` /
  `npm ci` / `go mod download` will re-fetch every layer and be slow. That is
  expected, not a failure.
- No listening ports, no background processes, no shell environment mutations
  you made by hand (anything outside what the platform injected).

## What survived

- `/workspace` in full — **checksum-verified against the snapshot manifest
  before you were started**. If it had not verified, you would not be running
  at all: the environment would be `Failed` with
  `RestoreVerificationFailed`.
- The agent home / session transcript (`$CLAUDE_CONFIG_DIR`).
- Caveat: file mtimes are not preserved, so `git status` re-hashes the
  working tree on the first run — expect it to take a moment.

## What you must NOT assume

None of these survive a wake:

- a `nohup`/`&` background process is still running;
- a dev server on `:3000` still listens;
- a container you started still holds its data;
- `npm ci` / `go mod download` caches are warm;
- anything outside `/workspace` and the agent home exists.

## Re-establish, in this order

1. Read the markers (`RESUME.md`, `last-freeze.json`, `last-wake.json`).
2. `git status` to reconcile the working tree against what you believe is
   checked out.
3. Restart any service containers you need (see the `use-docker` skill),
   expecting a slow first build.
4. Health-check anything before you rely on it.
5. **If a declared wait is what woke you, re-verify the condition actually
   holds** — do not assume it still does.

## How to reconcile against reality

`/v1/status` is the single authority for counters:
`freezeCount`, `wakeCount`, `phase`, `waitFor`, and `snapshot` (the latest
verified snapshot). `/etc/ai-sandbox/sandbox.json`'s `wake` block
(`{restored, snapshotID, seq}`) states which snapshot this pod was created
from — it is baked at render time and stable for the pod's whole life,
deliberately carrying no counters (those would be stale the instant they were
written).

The four on-disk markers:

| File | Author | Says |
|---|---|---|
| `.sandbox/RESUME.md` | freeze | prose: what was destroyed / what survived |
| `.sandbox/last-freeze.json` | freeze | machine form of the same |
| `.sandbox/last-wake.json` | restore | which snapshot, warm or cold, when, and whether the class/agent image changed since the snapshot |
| `.sandbox/warm-cache.json` | freeze/restore | internal warm-cache marker — informational only |

Plus: if a restore *had* failed you would not be reading this — the
environment would be `Failed`, and `status.restoreAttempt.reason` would name
the cause (e.g. `ChecksumMismatch`).

## The honest state of things

Wake restores `/workspace` and the agent home, byte-for-byte verified. It
does **not** restore the image cache, does **not** restart your containers,
and does **not** re-run your setup — re-establishing your own workload is
your job, because the platform cannot know which of your containers should
come back. `engine: none` is the only implemented engine today, so in this
deployment there are no workload containers for the platform to have torn
down anyway.

A cold wake (the warm PVC was reclaimed by TTL, or never existed) is an
optimisation miss, not an error — everything was still restored from S3.

**The real limit: nothing evaluates wait probes yet (#30).** A declared wait
does not clear itself — a human or a controller must clear `status.waitFor`
for a wake to be triggered at all. If you were woken, something did that;
if you are planning to declare a wait and be woken automatically, that does
not happen yet.
