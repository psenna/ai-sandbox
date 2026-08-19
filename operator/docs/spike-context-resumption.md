# Spike: Claude Code context resumption across a freeze → wake

Decision record for [#29](https://github.com/psenna/ai-sandbox/issues/29). Resolves the
hard gate the issue names: **before** any wake/restore code was written, prove that a
Claude Code session transcript — the agent's working memory — actually survives a
freeze → wake cycle, so that the operator's restore is restoring something real and
resumable, not just bytes that decrypt to nothing.

This mirrors the repo's existing spike precedent
([`spike-rootless-podman.md`](spike-rootless-podman.md)): a real experiment against the
stack's own components, run outside Kubernetes, with programmatic assertions and a
negative control, then committed as a decision record.

## Verdict

**Go.** A Claude Code session transcript, keyed by working directory, survives the
exact shape of a wake: the container is destroyed (new hostname, new container ID,
fresh `/tmp`), the config dir is carried as an archive, a fresh container restores it,
and the conversation resumes — both `claude -c` (continue most recent) and
`claude --resume <uuid>` recall a token seeded in the previous pod. A negative control
(fresh config dir, no restore) does **not** recall it, which is what makes the positive
result mean anything. No machine/container-identity-bound state breaks resumption, and
no credential is restored from disk — the stack injects model config through
environment, so the transcript is the only thing that has to cross the boundary.

The one honest operational caveat: a *fresh* `claude -p` (no `-c`/`--resume`) starts a
**new** session and does **not** pick up the prior transcript. Resumption is therefore
an explicit act — the woken agent (or its task prompt) must resume. That is exactly what
the `unfreeze` skill tells the agent to do; #29 guarantees the bytes are there to resume,
it does not auto-resume for the agent.

## The property under test

Claude Code keys its session transcripts by **working directory** under
`$CLAUDE_CONFIG_DIR/projects/<encoded-cwd>/<session-uuid>.jsonl`. The operator mounts
`/workspace` (`render.WorkspaceMountPath`) and sets `workingDir: /workspace` on every
agent pod (`render.RenderPod`), so the encoded key is **`-workspace`** on every pod, by
construction — identical across pods. The spike confirms that key is genuinely
cwd-derived (not hostname/container-ID-derived), that nothing machine-bound is stored
alongside the transcript in a way that invalidates it, and that the resulting file is
resumable.

## How it was run

The runnable script is committed at
[`operator/spike/context-resumption/run.sh`](../spike/context-resumption/run.sh) (with
a transient host helper, [`ollama-bridge.js`](../spike/context-resumption/ollama-bridge.js),
described below). It runs against the stack's **real** agent image shape — Claude Code
on `node:22-alpine`, built from the live `@anthropic-ai/claude-code` package — and the
stack's **real** model endpoint (`http://ollama:11434`,
`deepseek-v4-flash:0731-cloud`), outside Kubernetes. Every "pod" is a disposable
container launched by the sandbox's DinD daemon; the workdir and config dir are volumes
that survive the container (the way the workspace PVC and the restore would).

The procedure (programmatic assertions, nothing eyeballed):

1. **Run A** — fresh container A, fresh config dir, cwd `/workspace`:
   `claude -p "Remember this token: <RANDOM-32-hex>. Write it to
   /workspace/token-a.txt and confirm. Reply with exactly 'OK token stored'."`
   Assert stdout contains `OK token stored`. Capture the produced session UUID from
   `$CLAUDE_CONFIG_DIR/projects/-workspace/<uuid>.jsonl`.
2. **Freeze** — `tar` the config dir to an archive; container A was `--rm`, so it is
   already gone (new hostname, new container ID, fresh `/tmp` on the next run).
3. **Run B** — restore the config dir into a fresh mount point, launch a **new**
   container B against it, and run `claude -c -p "What token did I ask you to
   remember? Reply with ONLY the token, nothing else."` Assert stdout contains the
   exact token. Assert the **same** session file grew (the new turn was appended, not a
   new session created).
4. **Continuity** — `claude --resume <uuid> -p` (explicit resume by UUID) against the
   same transcript. Assert it recalls the token.
5. **Negative control** — fresh config dir, **no** restore, same question. Assert the
   token does **not** appear in the answer.

### The DinD networking gap (and the bridge)

A subtlety that is not about the property but about *running* the spike here: the spike's
claude containers are launched by the DinD daemon (`DOCKER_HOST=tcp://docker:2375`),
whose inner containers sit on the DinD bridge (`172.17.0.0/16`) and have **no route** to
the compose network where the ollama model endpoint lives (`172.21.0.0/24`). Measured
directly:

```
$ # from a DinD inner container:
$ docker run --rm --add-host=ollama:172.21.0.3 curlimages/curl \
    -s --max-time 12 -w '[http_code=%{http_code}]\n' http://ollama:11434/
[http_code=000]              # connection timeout — unreachable
$ docker run --rm alpine sh -c 'ping -c1 -W2 172.21.0.3'   # 100% packet loss
```

The stack's `claude` container bridges both networks (it is on the dependaproxy net,
which DinD *can* route to, and the ollama net). So the script starts a tiny TCP forwarder
— `ollama-bridge.js` — **on this host**, listening on a DinD-routable IP and forwarding
to `ollama:11434`, points the spike's `ollama` hostname at it, and tears the bridge down
on exit. This is a transient host helper for the spike run, not operator code; it is
committed only so the spike is self-contained and re-runnable in this stack. (In a
deployment where the workload network can already reach the model endpoint, the bridge
is unnecessary and the script's auto-detection falls back to a direct route.)

## Results (verbatim, from one passing run)

```
== bridging ollama to DinD via 172.23.0.3:11434 -> ollama:11434
== token: 03ec648ce4588151e3969ee7b0e7a49d
== [1/5] Run A: seed the transcript in container A
OK token stored
== session uuid: 6e96e29f-6ee5-42fd-99ac-8f11bd6302c2
== [2/5] Destroy container A (it was --rm: gone) and freeze the config dir
session file size after run A: 13588 bytes
== [3/5] Restore the config dir into a fresh mount point, then run B
    from a NEW container (new hostname/ID, fresh /tmp): the exact wake shape.
    NOTE: -c continues the most recent session -- the only way the wake
    can recall: a fresh session has no memory of the previous pod.
03ec648ce4588151e3969ee7b0e7a49d
session file size after run B: 16915 bytes
== [4/5] Continuity: claude --resume <uuid> -p ... against the same transcript
03ec648ce4588151e3969ee7b0e7a49d
== [5/5] Negative control: fresh config dir (no restore), same question, no recall
I have no token stored — my memory is empty. I don't recall any token you asked me to remember.
== PASS: transcript survives freeze -> tar -> fresh container -> wake
   (session 6e96e29f-6ee5-42fd-99ac-8f11bd6302c2, 13588 -> 16915 bytes, --resume OK, negative control clean)
SPIKE_EXIT=0
```

(The `claude-code:unrecognized_model` warning lines that appear in the raw log —
`"deepseek-v4-flash:0731-cloud" is not a model this version of Claude Code recognizes,
so auto-compact will keep this session within 200k tokens` — are emitted to stderr and
elided here; see "Observations" below.)

Each assertion passed programmatically:

| # | assertion | result |
|---|---|---|
| 1 | run A confirms `OK token stored` | ✅ |
| — | a session file exists under `projects/-workspace/<uuid>.jsonl` | ✅ `6e96e29f-…` |
| 3 | run B (`-c`, fresh container, restored dir) stdout contains the exact token | ✅ `03ec648c…` |
| 3b | the same session file grew (turn appended, not a new session) | ✅ 13588 → 16915 bytes |
| 4 | `--resume <uuid>` stdout contains the exact token | ✅ `03ec648c…` |
| 5 | negative control stdout does **not** contain the token | ✅ "my memory is empty" |

## Observations (recorded, not asserted)

- **Auth is env-injected, not disk-bound.** The restored config dir contains **no**
  `.credentials.json` and **no** `statsig/` artifacts. The stack injects model config
  (`ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`, `ANTHROPIC_MODEL`) through the rendered
  Secret's `envFrom`, so nothing about identity has to be restored from disk — exactly
  the design assumption #29 relies on. (Confirmed by `find … -iname '*credentials*' -o
  -iname '*statsig*'` returning nothing.)
- **File ownership after restore is uid/gid 1000.** The transcript
  `projects/-workspace/6e96e29f-….jsonl` and `work-a/token-a.txt` are both `uid=1000
  gid=1000`, matching the pod's `runAsUser`/`fsGroup: 1000`. (The real restore's
  `ReadArchive` zeroes uid/gid and the pod's `fsGroup` reconciles; here the containers
  ran as `1000:1000` throughout, so ownership was already correct.)
- **The session-encryption key travels inside the config dir.** Claude Code writes a
  per-config-dir session key under `$CLAUDE_CONFIG_DIR/sessions/`; it is not
  machine-bound, so it is carried by the archive and decryption works in the fresh
  container — proven indirectly by the fact that `-c` and `--resume` *read* the
  encrypted transcript and recalled the token.
- **The "unrecognized model" warning is harmless here.** Claude Code 2.1.233 does not
  have `deepseek-v4-flash:0731-cloud` in its built-in model registry, so it emits a
  stderr warning and conservatively assumes a 200k-token auto-compact window. The model
  is served correctly (the stack's ollama proxy maps the name), and the warning does not
  affect correctness — the spike's recall is far under any context window. It is a real
  property of this stack's model setup worth knowing for production (the agent runs
  under a model Claude Code does not "recognize"), but it is not a resumption concern.
- **gzip, not zstd, for the spike's archive.** The host running the script (the `claude`
  container) has no `zstd` binary and its BusyBox `tar` has no `--zstd`, so the spike's
  "freeze" step uses `tar -czf` (gzip). The real operator's freeze uses zstd
  (`internal/storage/archive.go`); the archive format is incidental to what the spike
  proves (that the bytes survive and resume), so this divergence is noted, not a gap.

## What this proves

- The session transcript is **cwd-keyed** (`projects/-workspace/<uuid>.jsonl`), not
  keyed by hostname, container ID, or any machine identity. The operator's constant
  `workingDir: /workspace` therefore produces an **identical** project key across pods
  by construction — which is the entire premise of wake/restore.
- The transcript **survives** a destroy → archive → fresh-container → restore cycle,
  intact and decryptable, with no machine-bound state invalidating it.
- The transcript is **resumable** — both `claude -c` (continue most recent) and
  `claude --resume <uuid>` load it and the model recalls prior context. The same session
  file grows (a real continuation), confirming it is the same conversation, not a
  coincidental new one.
- The negative control **isolates the transcript as the cause**: without the restored
  bytes, the same question yields no recall.

## What this does not prove

- **It does not prove auto-resumption.** A fresh `claude -p` (no `-c`/`--resume`) starts
  a *new* session and does not load the prior transcript — the woken agent must resume
  explicitly. This is by design (the `unfreeze` skill instructs the agent to do so);
  #29 guarantees the bytes are present and resumable, it does not resume for the agent.
- **It is one model, one Claude Code version** (`2.1.233`), one run, outside Kubernetes.
  It proves the mechanical property #29 depends on; it is not a statistical claim about
  every model. The automated regression
  ([`test/e2e/resumption_test.go`](../test/e2e/resumption_test.go)) guards the property
  going forward without needing a model, by asserting the agent home's bytes survive
  freeze → S3 → wake into a fresh pod **and are readable by the agent process itself**
  (assertions run inside the woken pod).
- **It does not exercise the operator's restore path** (warm marker, S3 manifest
  verification, checksums). Those are covered by the e2e wake suite
  ([`test/e2e/wake_test.go`](../test/e2e/wake_test.go)). This spike is the *gate* that
  says "restoring those bytes is worth doing at all."

## Go / no-go

**Go.** Resumption is not lossy: the project key is cwd-derived, the transcript survives
the wake shape, and it is resumable. The #29 storage format and restore flow are built on
a property that holds. The two layers are named as such in the PR: **Layer 1** (this
spike) is the finding that the property holds; **Layer 2** (the e2e regression) is the
guard that keeps it true.