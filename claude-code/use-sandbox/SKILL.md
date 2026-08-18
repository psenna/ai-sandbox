---
name: use-sandbox
description: Use when the agent is running inside an ai-sandbox operator SandboxEnvironment and needs to tell the platform it is blocked ("I am waiting on CI"), that it has finished, or to leave a progress breadcrumb. A localhost-only control API at http://localhost:9099 is the ONLY channel to the operator; the agent has no Kubernetes credential and no cluster access. Use it any time a task says to wait for an external condition (a CI pipeline, a URL, an object appearing, a time), to report completion or failure, or whenever you would otherwise idle-poll or give up.
---

# Use sandbox (sandboxctl control API)

You are running inside a Kubernetes pod managed by the ai-sandbox operator. The
pod has **two containers**: this one (you), and a sidecar called `sandboxctl`
that holds the pod's Kubernetes credential. **You do not have a Kubernetes
credential, `kubectl`, or any cluster access, by design.** The only way to
influence your `SandboxEnvironment` object is through `sandboxctl`'s HTTP API,
which validates every request against a strict allowlist before writing
anything.

Base URL: `$SANDBOX_SIDECAR_URL` (defaults to `http://localhost:9099`, already
set in your process environment). Also available at the `sidecarURL` key of
`/etc/ai-sandbox/sandbox.json`. The API is bound to loopback only — it is not
reachable from outside this pod, and nothing else in the cluster can reach it
either.

## The three things you can say

### 1. "I'm blocked, wake me later" — `POST /v1/wait`

Use this when your task genuinely cannot proceed until something external
happens (CI finishes, a deploy completes, a cooldown period passes). Declaring
a wait **freezes your sandbox** — after a 2xx response, stop working and exit;
you will not be resumed until the condition is met (see "Freezing has a real
cost" below).

```sh
curl -sS -X POST "$SANDBOX_SIDECAR_URL/v1/wait" \
  -H 'Content-Type: application/json' \
  -d '{
        "type": "GitProxyCheck",
        "reason": "waiting for CI on the feature branch",
        "params": {"ref": "refs/heads/feat/27-sandboxctl"}
      }'
```

A 2xx response means the wait was recorded — exit now. A 4xx means it was
rejected; see the error table below and fix the request. **Only declare a wait
after you get a 2xx back** — never assume it worked.

### 2. "I'm done" — `POST /v1/done`

Report exactly one outcome per run. `outcome` is `"success"` or `"failure"`.

```sh
curl -sS -X POST "$SANDBOX_SIDECAR_URL/v1/done" \
  -H 'Content-Type: application/json' \
  -d '{"outcome": "success", "message": "opened PR #47", "exitCode": 0}'
```

Reporting the exact same result twice is fine (idempotent, returns `200`
instead of `202`). Reporting a *different* result the second time is rejected
(`409 result_already_reported`) — you get one outcome per run.

### 3. "Still working, here's a breadcrumb" — `POST /v1/progress`

Purely informational: written to the sidecar's own log
(`kubectl logs <pod> -c sandboxctl`, if anyone is watching) and kept in a
small in-memory buffer visible on `/v1/status`. It is NOT written to
Kubernetes status and does not affect your environment's phase.

```sh
curl -sS -X POST "$SANDBOX_SIDECAR_URL/v1/progress" \
  -H 'Content-Type: application/json' \
  -d '{"message": "cloned repo, running tests"}'
```

### Checking your own status — `GET /v1/status`

```sh
curl -sS "$SANDBOX_SIDECAR_URL/v1/status"
```

Returns your environment's phase, whether a wait/result has been recorded, and
recent progress breadcrumbs — served from a cache refreshed every few seconds,
never a live cluster call. **Do not poll this in a tight loop** (see Rules).

## Probe types (`POST /v1/wait`'s `type` + `params`)

`type` must be exactly one of these four — anything else is rejected
(`400 unknown_probe_type`). Params not listed for a type are rejected, not
silently dropped (`400 unknown_param`).

| type | params | example |
|---|---|---|
| `GitProxyCheck` | `ref` (required, a git ref e.g. `refs/heads/feat/x`); `repo` (optional, `"owner/name"`, defaults to your environment's own repo) | `{"type":"GitProxyCheck","reason":"waiting for CI","params":{"ref":"refs/heads/feat/x"}}` |
| `HTTPGet` | `url` (required, absolute `http(s)://`, no embedded credentials); `expectStatus` (optional, `100`-`599`, default `200`); `expectBody` (optional, substring the body must contain) | `{"type":"HTTPGet","reason":"waiting for deploy","params":{"url":"https://example.com/health","expectStatus":"204"}}` |
| `S3ObjectExists` | `key` (required, object key relative to your environment's backend prefix; no leading `/`, no `..`) | `{"type":"S3ObjectExists","reason":"waiting for build artifact","params":{"key":"builds/1.tar.zst"}}` |
| `NotBefore` | exactly one of `time` (RFC3339 timestamp) or `duration` (Go duration string, e.g. `"30m"`, max `24h`) | `{"type":"NotBefore","reason":"cooldown","params":{"duration":"30m"}}` |

`reason` is always required (a short human-readable string, ≤512 bytes, no
control characters) on every `POST /v1/wait` call, regardless of type.

## Honesty about what happens after you declare a wait

**Declaring a wait freezes your sandbox, but nothing currently evaluates
whether the condition is satisfied.** That evaluator is a separate, not-yet-
shipped piece of the platform. Today, a declared wait holds your environment
in a frozen/waiting state indefinitely — it will not automatically resume
when your condition becomes true. Do not plan around "it'll wake me up when
CI passes" actually happening yet. If your task truly cannot make progress
without that automatic wake-up, say so in your final report rather than
silently declaring a wait and stopping.

## Freezing has a real cost

Freezing is not free or instant — it involves snapshotting your workspace and
tearing down the running container. That machinery is also still being built
out; for now, treat freezing as a one-way trip for the remainder of this
skill's scope. A future `freeze` skill will cover what to expect in more
detail once that machinery lands.

## Errors

Every non-2xx response has the same shape:

```json
{"error": {"code": "unknown_probe_type", "message": "...", "field": "type", "allowed": ["GitProxyCheck", "HTTPGet", "NotBefore", "S3ObjectExists"]}}
```

| code | meaning | what to do |
|---|---|---|
| `bad_json` | body is not valid JSON, or has trailing data | fix the JSON |
| `unknown_field` | body has a field the API doesn't recognize | remove it; check spelling against this skill |
| `payload_too_large` | body exceeds the endpoint's size limit | shorten `reason`/`message`/params |
| `unsupported_media_type` | missing/wrong `Content-Type` | send `Content-Type: application/json` |
| `method_not_allowed` | wrong HTTP method for the path | check the method in this skill |
| `not_found` | no such endpoint | check the path |
| `unknown_probe_type` | `type` is not one of the four allowlisted values | use one from the table above; see `allowed` in the response |
| `missing_param` | a required param (or `reason`) was omitted | add it |
| `unknown_param` | a param not permitted for this `type` | remove it; check the table above |
| `invalid_param` | a param failed validation (bad URL, bad duration, etc.) | fix the value; the `message` says why |
| `invalid_outcome` | `/v1/done`'s `outcome` was not `"success"`/`"failure"` | use one of those two strings |
| `wait_already_declared` | a wait was already declared for this run | you already declared a wait; stop calling this endpoint |
| `result_already_reported` | `/v1/done` was already called with a DIFFERENT result | you get one outcome per run; this call is rejected |
| `freezing` | your environment has started freezing | stop calling the API; you are about to be torn down |
| `rate_limited` | you called an endpoint too fast | back off; see the rate limits below |
| `status_patch_failed` | the sidecar could not reach the API server | transient; retry the same request |

## Rules

- **Never poll `/v1/status` in a tight loop.** It is rate-limited (5
  requests/second, small burst). Poll at most a few times a minute if you
  need to check your own state.
- **Never try to reach the Kubernetes API server directly.** You have no
  credential for it (no token file, no `kubectl`), and this is intentional —
  do not look for a way around it.
- **Never assume a `/v1/wait` or `/v1/done` call succeeded unless you got a
  2xx back.** A network hiccup or validation failure means nothing was
  recorded; check the response.
- **Exit promptly after a successful `/v1/wait` or `/v1/done`.** Continuing
  to work after declaring a wait or reporting a result wastes the run — the
  environment is about to be frozen or reclaimed regardless of what you do
  next.
