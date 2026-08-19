---
name: freeze
description: Use when the agent is about to declare a wait (POST /v1/wait), has been told its environment may be suspended, or is resuming after a freeze and needs to know what is no longer true. Freezing snapshots /workspace and the agent home, then destroys the running pod and every container it started.
---

# Freeze (what actually happens when you declare a wait)

You are running inside a Kubernetes pod managed by the ai-sandbox operator.
When you (or the platform, via `spec.suspend` or a timeout) trigger a
**freeze**, the operator sidecar (`sandboxctl`) snapshots your workspace, then
**destroys the pod**. This skill tells you, honestly, what that means for you
right now and, if you're reading this after a resume, what to check before you
trust anything.

Freeze is implemented, and so is **wake/restore**: a frozen environment can
be resumed, its `/workspace` and agent home restored into a fresh pod. See
"The honest state of things" below for what that does and — just as
importantly — does not bring back.

## What freeze is

1. Every container you started (via the sandbox's container engine, if any)
   is stopped and removed — **before** anything is snapshotted. A tar taken
   while a container is still writing to `/workspace` would be torn, so
   teardown always happens first.
2. `/workspace` and your agent home (`$CLAUDE_CONFIG_DIR`,
   `/home/node/.claude-sandbox`) are archived and uploaded.
3. The pod is deleted and your scheduling slot is released — another queued
   environment can start in your place.

## What is destroyed

- **Every container you started.** Stopped and removed before the snapshot,
  not paused — nothing about them survives.
- **The container image/layer cache.** Never snapshotted, by design (it's
  large, reconstructable, and not part of your task's state).
- **Anything outside `/workspace` and the agent home**: installed OS
  packages, `/tmp`, background processes, listening ports, environment
  variable mutations you made by hand (outside what the platform injected).
  If you `apt install`'d something, `nohup`'d a server, or edited a dotfile
  in `$HOME` outside the agent home, it is gone.

## What survives

- `/workspace` in full, including `.git`, `.claude/`, and any files you
  wrote — this is the one thing you can rely on.
- The agent home / session transcript.
- A marker written at `/workspace/.sandbox/last-freeze.json` (machine-
  readable) and `/workspace/.sandbox/RESUME.md` (prose) describing exactly
  what this freeze destroyed and preserved. **This marker is the authority on
  what survived, not your memory of this skill or your own transcript** — it
  is generated fresh at freeze time from what actually happened.

## Before you declare a wait

Declaring a wait (`POST /v1/wait`, see the `use-sandbox` skill) triggers a
freeze as soon as the sidecar observes it. Before you call it:

- **Commit and push anything valuable.** Uncommitted changes survive in
  `/workspace` today (freeze doesn't wipe the working tree), but don't rely on
  that being your only copy — push what matters.
- **Write down, in `/workspace`, anything you're holding only in your own
  reasoning.** Your context resets on the next run even after a wake; a note
  in a file is the only thing that reliably crosses that boundary.
- **Don't leave a task half-applied across a container you assume is still
  running.** If step 2 of a plan depends on a background process from step 1,
  finish or checkpoint it first — that process will not survive.
- Only declare a wait after a 2xx response, and exit immediately after —
  see `use-sandbox` for the exact mechanics.

## On resume (if you are reading this after one)

If you're running and `/workspace/.sandbox/RESUME.md` exists, **read it
first**, before trusting your own transcript. It states, for the freeze that
produced it: what was destroyed, what survived, and what to re-create. Never
assume a container, listening port, or installed package from before the
freeze is still there just because your conversation history mentions it —
the transcript describes what you *did*, not what *survived*. Re-create any
service you need before relying on it.

Then read the **`unfreeze` skill** — it is the counterpart to this one, and
tells you exactly what the wake restored (and did not), how to tell warm from
cold, and what to re-establish and in what order.

## The honest state of things

Freeze (snapshot, teardown, slot release) is implemented and works today.
**Wake/restore is implemented too** (#29): a frozen environment can be
resumed into a fresh pod, with `/workspace` and the agent home restored and
checksum-verified against the snapshot manifest before you start. That does
**not** mean everything comes back: the image cache is cold (your first build
re-pulls every layer), every container you started is gone (re-establishing
your own workload is your job), and nothing evaluates wait probes yet (#30) —
a declared wait does not clear itself, so a human or a controller must clear
`status.waitFor` for a wake to be triggered at all. If your task needs a
wake, say so in your `/v1/done` report rather than assuming it will happen
automatically.
