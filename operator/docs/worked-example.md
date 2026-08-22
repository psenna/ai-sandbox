# Worked example: one agent, one issue, one merged PR

> **Not executed in CI.** The [quickstart](../README.md#quickstart) is (see
> `.github/workflows/docs.yml`); this walkthrough is not. It needs a real
> model endpoint, a real git-proxy holding a real upstream PAT, and write
> access to a real repository — none of which a CI runner has or should
> have. The outputs transcribed below are the **shapes** the operator
> produces — drawn from the e2e suite's doubles and envtest fixtures,
> rather than transcribed from a single live run against a real repository.
> The section ["What of this is mechanically verified"](#what-of-this-is-mechanically-verified)
> at the end says exactly which mechanisms *are* covered by tests, and by
> which ones.

## The scenario

One namespace, one `SandboxClass`, one `SandboxEnvironment`: an agent is
pointed at `psenna/e2e-fixture` issue `#N`, implements it, opens a PR
through git-proxy, waits for CI without holding a slot, wakes when CI goes
green, merges, and reports success.

## Prerequisites

- An operator install (Helm — see [`operations.md`](operations.md#installing)).
- git-proxy and its broker reachable in-cluster (`<svc>.<ns>.svc`).
- A real model endpoint (Ollama or an Anthropic-compatible one).
- An S3-compatible endpoint (freeze/wake/archive are S3-only).
- A real agent image — one that knows the `use-git-proxy` broker contract
  and speaks the sidecar's control API (`use-sandbox` skill).

## Step 0 — the credentials you create yourself

Two Secrets, both in the operator's `--class-secret-namespace` (**not** the
environment's namespace — see [`operations.md`](operations.md#the-s3-credentials-trap)):

```sh
kubectl -n ai-sandbox-operator-system create secret generic git-proxy-bearer \
  --from-literal=token=<the AGENT_TOKEN broker bearer, NOT the upstream PAT>

kubectl -n ai-sandbox-operator-system create secret generic sandbox-s3-credentials \
  --from-literal=accessKeyID=<key> \
  --from-literal=secretAccessKey=<secret>
```

The operator reads these when resolving `services.gitProxy.tokenSecretRef`
and `storage.backend.s3.credentialsSecretRef`, and projects the resulting
values into the environment's own rendered Secret — the operator itself
never logs or mints a credential (see [`security.md`](security.md)). The
agent ends up holding the git-proxy bearer (never the PAT) and, if the
engine needed it, nothing S3-related at all — S3 credentials go only to the
`sandboxctl`/`restore` containers.

## Step 1 — the `SandboxClass`

```yaml
apiVersion: sandbox.psenna.dev/v1alpha1
kind: SandboxClass
metadata: {name: real-agent}
spec:
  agent:
    image: ghcr.io/psenna/ai-sandbox-agent:latest
    resources:
      requests: {cpu: 500m, memory: 512Mi}
  engine:
    type: none
  services:
    gitProxy:
      url: http://git-proxy.ai-sandbox.svc.cluster.local:8080
      brokerURL: http://git-proxy.ai-sandbox.svc.cluster.local:8090
      tokenSecretRef: {name: git-proxy-bearer}
    dependaProxy:
      url: http://dependaproxy.ai-sandbox.svc.cluster.local:8080
    ollama:
      url: http://ollama.ai-sandbox.svc.cluster.local:11434
  storage:
    workspace: {size: 2Gi}
    backend:
      type: s3
      s3:
        endpoint: https://s3.example.com
        bucket: sandbox-snapshots
        credentialsSecretRef: {name: sandbox-s3-credentials}
  network:
    isolation: Restricted
    extraEgress:
      - cidr: 0.0.0.0/0
        ports: [{port: 443, protocol: TCP}]
  timeouts: {running: 6h, waiting: 24h, total: 72h}
EOF
```

**Callout:** with `network.isolation: Restricted`, every service endpoint
must be an in-cluster `<svc>.<ns>.svc` host (resolved to that Service's own
selector) or covered by an `extraEgress` CIDR — otherwise the class is
rejected at observe time with `ResourcesNotReady`. The `extraEgress` entry
above is deliberately broad (any HTTPS destination) because a real coding
agent's outbound needs are open-ended; narrow it to your actual endpoints
where you can.

## Step 2 — `kubectl apply` the `SandboxEnvironment`

```yaml
apiVersion: sandbox.psenna.dev/v1alpha1
kind: SandboxEnvironment
metadata: {name: fix-flaky-test, namespace: ai-sandbox}
spec:
  classRef: {name: real-agent}
  repo: psenna/e2e-fixture
  task:
    issueRef: {repo: psenna/e2e-fixture, number: 42}
```

**Honest callout, verified in `internal/render/configmap.go`:** `issueRef`
does **not** fetch the issue. `task.md` is rendered as literally `Implement
issue #42 in psenna/e2e-fixture.` The **agent** fetches the issue itself
through the broker, using `GIT_PROXY_BROKER_URL` + `AGENT_TOKEN` — the same
`use-git-proxy` skill contract as the compose stack. The operator renders
**no** skill files into the pod: `Inputs.Skills` is never populated by any
controller code path. Your agent image must already know how to work with
the broker.

## Step 3 — watching it run

```console
$ kubectl -n ai-sandbox get sandboxenvironment fix-flaky-test -w
NAME             PHASE      READY   AGE
fix-flaky-test   Pending    False   1s
fix-flaky-test   Ready      False   2s
fix-flaky-test   Restoring  False   3s
fix-flaky-test   Running    True    14s
```

The operator's own log, at `--log-verbosity=1`, shows each `POST
/v1/progress` the sidecar relays from the agent as it works.

## Step 4 — the agent opens a PR

The agent clones through git-proxy, makes its change, pushes a `feat/*`
branch, and opens a PR through the broker — exactly the `use-git-proxy`
skill flow used elsewhere in this repo. On the next freeze (Step 5), the
sidecar reads `/workspace/.sandbox/git-state.json` (written by the agent)
and persists `status.gitState.{branch,headSHA,pullRequest}` onto the
`SandboxEnvironment`.

## Step 5 — waiting for CI without burning a slot

The agent declares a wait through the sidecar's loopback API:

```
POST http://127.0.0.1:9099/v1/wait
{"type":"GitProxyCheck","params":{"repo":"psenna/e2e-fixture","ref":"refs/heads/feat/fix-flaky-test"}}
```

This drives the phase walk `Running -> Freezing -> Waiting`: the sidecar
quiesces and snapshots the workspace and agent home, the pod is deleted,
and — critically — the **slot is released** the moment the pod is gone, not
when the wait clears. A waiting environment costs no execution slot, only
its retained workspace PVC (until warm-cache TTL GC reclaims it) and its S3
snapshot bytes.

The operator's probe evaluator then polls the `GitProxyCheck` on its own
schedule: backoff 1s -> 2s -> 4s -> 8s -> 8s… with +-20% jitter. Three
consecutive **unevaluatable** results (not "still pending" — actual
errors: bad URL, broker auth failure) fail the environment; a merely-still-
running CI check just keeps polling.

## Step 6 — CI goes green, the sandbox wakes

```console
$ kubectl -n ai-sandbox get sandboxenvironment fix-flaky-test
NAME             PHASE      READY
fix-flaky-test   Ready      False
```

`ProbeSatisfied` -> `status.waitFor` cleared -> `Ready` -> `Restoring`. The
`restore` init container (present only on S3-backed classes, ordered last,
non-restartable) checksum-verifies and restores the workspace and agent
home before the `agent` container starts at all — see
`status.restoreAttempt.roots[]` for whether each root restored `Warm`
(PVC still present and validated, zero bytes downloaded) or `Cold` (fresh
download from S3).

**What the wake does NOT restore:** the container image cache, any
containers the agent itself started, `/tmp`, and any packages installed
outside `/workspace`/agent-home. The agent reconciles against this reality
by reading the four `.sandbox/` markers left by freeze/restore
(`RESUME.md`, `last-freeze.json`, `last-wake.json`, `warm-cache.json`) — see
`../README.md`'s Wake/restore design notes for the full marker table.

## Step 7 — merge, and Done

The agent merges the PR through the broker, then reports:

```
POST http://127.0.0.1:9099/v1/done
{"outcome":"Succeeded"}
```

-> `Done` -> the terminal archive Job runs -> `Archived=True`.

## Step 8 — the receipts

```console
$ kubectl -n ai-sandbox get sandboxenvironment fix-flaky-test \
    -o jsonpath='{.status.archive.uri}{"\n"}{.status.archive.runJSONSHA256}{"\n"}'
s3://sandbox-snapshots/prod-cluster/ai-sandbox/fix-flaky-test/3f9a.../archive/
9f2c1a...  (sha256 of run.json)

$ mc cp minio/sandbox-snapshots/prod-cluster/ai-sandbox/fix-flaky-test/3f9a.../archive/run.json -
```

`run.json` (`internal/storage/runrecord.go`) carries the full record: the
resolved spec, phase history with timestamps, every snapshot taken, the git
state (branch/headSHA/PR), timing, and `context.present`/`context.reason`
for the archived transcript. Verify the download against
`status.archive.runJSONSHA256` before trusting it.

## Step 9 — cleanup, retention, and what gets deleted when

See [`operations.md`](operations.md#retention-and-cost) for the full
retention model — `--retention-ttl` (default 168h) and orphan cleanup are
what eventually reclaim this environment's storage root, on a schedule
independent of anything covered above.

## What of this is mechanically verified

| Mechanism | Covered by |
|---|---|
| Phase walk to `Done` | `test/e2e/lifecycle_test.go` ("reaches Done for a script that writes a file and exits 0") |
| `/v1/done` with no Kubernetes credential in the agent container | `test/e2e/lifecycle_test.go` ("reaches Done via /v1/done with no Kubernetes credential in the agent container") |
| Control API is loopback-only | `test/e2e/lifecycle_test.go` ("does not expose the control API outside the pod") |
| Wait declared -> `Freezing` -> slot released | `test/e2e/lifecycle_test.go` ("records an agent-declared wait …") |
| `GitProxyCheck` probe -> satisfied | `test/e2e/probe_test.go`, against the fake broker double, for `psenna/e2e-fixture` refs |
| Freeze snapshot + failure hold | `test/e2e/freeze_test.go` |
| Warm/cold wake, corrupt snapshot | `test/e2e/wake_test.go`, `test/e2e/resumption_test.go` |
| `Restricted` NetworkPolicy rendering | `internal/controller/networkpolicy_test.go` |
| CNI actually enforces | `test/e2e/netpolicy_test.go`, `test/e2e/isolation_test.go` |
| Install / upgrade / uninstall / reinstall | `operator/hack/helm-kind-walkthrough.sh` |

**Plainly: nothing here verifies against a real GitHub repository, a real
PAT, or a real model.** The e2e suite's broker and model are doubles
(`test/e2e/doubles/`); the "PR" they return is canned JSON, and the "model"
is a stub that returns `StopReason: "end_turn"`. Claiming this walkthrough
as CI-verified end to end would be exactly the dishonesty this repo's own
security documentation pushes against — hence the banner at the top of this
page.
