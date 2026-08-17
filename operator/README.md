# ai-sandbox operator

Kubernetes operator for ai-sandbox. See [issue #15](https://github.com/psenna/ai-sandbox/issues/15)
for the broader design context.

This directory started as a **scaffold only** (issue #16): a Go module,
kubebuilder v4 project layout, CI, and a manager that starts, serves health
probes, and exits cleanly. [Issue #17](https://github.com/psenna/ai-sandbox/issues/17)
added the `sandbox.psenna.dev/v1alpha1` API types (`SandboxClass`,
`SandboxEnvironment`), their CRDs, and envtest-backed validation/round-trip
tests. [Issue #18](https://github.com/psenna/ai-sandbox/issues/18) added the
`SandboxEnvironment` phase state machine as a pure function
(`internal/lifecycle`), the reconciler that drives it against a real cluster
(`internal/controller`), and the RBAC generated from its
`+kubebuilder:rbac` markers. The reconciler's `Observe` seam is deliberately
a stub today (`ObserveStub`): no child resources, pods, snapshots, or probes
exist yet, so a new environment settles in `Pending` and stays there. Those
subsystems land incrementally in #19, #20, #21, #24, #27, #28, #29, #30 and
#32, each replacing one piece of `ObserveStub` and one `notImplemented`
action without touching the state machine or the reconcile loop itself.

## Layout

```
operator/
├── cmd/main.go              entrypoint: config, manager, signal handling
├── api/v1alpha1/            SandboxClass / SandboxEnvironment API types
├── internal/config/         flag/env configuration surface
├── internal/operator/       manager construction (scheme, options, probes)
├── internal/lifecycle/      pure SandboxEnvironment phase state machine (no controller-runtime import)
├── internal/controller/     the reconciler: Observe -> lifecycle.Next -> actions -> status write
├── internal/apitest/        envtest-backed API validation/round-trip tests
├── config/                  kustomize bases (CRDs, manager Deployment, RBAC)
├── config/samples/          example SandboxClass/SandboxEnvironment YAML
├── hack/                    boilerplate header, smoke test, tool scripts
├── hack/tools/              separate Go module that builds controller-gen
├── Dockerfile               multi-stage, cross-compiled, distroless
└── Makefile                 build/test/lint entrypoints (see below)
```

## Make targets

All Go/make commands run inside a `golang:1.25` container against the
sandbox's Docker-in-Docker daemon — there is no Go toolchain or `make` on
the agent host. Set `IN_CONTAINER=1` when `make` itself already runs inside
a container with Go installed (this is what CI and the golang container do);
leave it unset (default `0`) to have the Makefile wrap every command in its
own `docker run` against `$DOCKER_HOST`.

| target | purpose |
|---|---|
| `build` | compile `bin/manager` |
| `test` | `go test -race` with coverage |
| `vet` | `go vet ./...` |
| `fmt` / `fmt-check` | `gofmt -w` / drift check |
| `lint` | `golangci-lint run` |
| `vuln` | `govulncheck` |
| `tidy` | `go mod tidy` |
| `manifests` | CRD manifests (`config/crd/bases/*.yaml`) and RBAC (`config/rbac/role.yaml`) via `controller-gen`, built from `hack/tools` |
| `generate` | deepcopy generation (`api/v1alpha1/zz_generated.deepcopy.go`) via `controller-gen` |
| `envtest-assets` | download and checksum-verify real kube-apiserver/etcd/kubectl binaries for envtest |
| `test-envtest` | `go test -race` against `internal/apitest` and `internal/controller`, using the envtest binaries above |
| `kustomize-build` | render `config/default` |
| `cross` | cross-compile linux/amd64 and linux/arm64 |
| `docker-build` | build the operator image |
| `smoke` | build then run `hack/smoke.sh` — starts the manager outside a cluster, checks `/healthz` and `/readyz`, and confirms clean shutdown on SIGTERM |
| `clean` | remove build artifacts |

## Dependency versions

Go module versions in `go.mod` are pinned rather than left to `go mod tidy`
to pick freely: dependencies are fetched through DependaProxy
(`http://dependaproxy:8080/goproxy`), which enforces a minimum publication
age and denies known-CVE-affected versions. The pinned versions here were
confirmed to pass both gates at the time this scaffold was written.

A few versions differ from the initial plan because the plan's pins do not
build together, or a transitive dependency was denied by DependaProxy's
CVE gate:

- `sigs.k8s.io/controller-runtime` is `v0.23.3`, not `v0.24.1`: `v0.24.x`
  declares `go 1.26.0`, which the `golang:1.25` toolchain in use here
  cannot satisfy. `v0.23.3` is the highest release that declares `go
  1.25.0`.
- `k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go` are `v0.35.0`,
  not `v0.36.3`, to match the `k8s.io/*` versions controller-runtime
  `v0.23.3` itself requires (`v0.36.x` also declares `go 1.26.0`).
- `golang.org/x/net`, `golang.org/x/sys`, `golang.org/x/term`,
  `golang.org/x/text` and `github.com/klauspost/compress` are pinned as
  explicit indirect requires, each bumped to the lowest version that
  clears DependaProxy's CVE gate (the versions MVS would otherwise select
  from controller-runtime/client-go's own requirements are denied).
  `go mod tidy` needed `-e` to complete: `golang.org/x/mod` is denied by
  DependaProxy's CVE gate (`GO-2026-6179`/`GO-2026-6180`) at every
  released version, with no passing release available at the time of
  writing. `x/mod` only enters the graph as a *test* dependency of
  `sigs.k8s.io/controller-runtime`'s own test suite (via
  `ginkgo`→`golang.org/x/tools`→`golang.org/x/mod`) — this repository
  never imports it. `go build`, `go vet` and `go test ./...` all succeed
  fully offline against the committed `go.sum` (verified with
  `GOPROXY=off -mod=readonly`), confirming nothing on the real build path
  is missing.
- The same `golang.org/x/mod` gap blocks `go install` of `golangci-lint`,
  `govulncheck`, `controller-gen` and `kustomize` outright: each pulls in
  `golang.org/x/tools`, which requires `golang.org/x/mod`, and `go install
  pkg@version` builds against that tool's own pinned dependency graph, so
  this project's `go.mod` requires can't bump it out of the way the way
  they can for our own module. `make lint`, `make vuln` and
  `make kustomize-build` therefore still cannot succeed in this
  environment — not a code or Makefile defect, but an upstream/proxy gap
  (no `golang.org/x/mod` release currently clears DependaProxy's CVE
  gate). These are expected to start working again once a patched `x/mod`
  release is available through DependaProxy.

### `make manifests` / `make generate`: `hack/tools`

Unlike `go install pkg@version`, a Go **module**'s own `go.mod` requires
*can* override a dependency's transitive pin via MVS (minimal version
selection). `hack/tools/` is a separate Go module (never referenced from
`operator/go.mod`, never part of `operator`'s own build, test, or
`govulncheck` scan) whose only purpose is building `controller-gen` from
source, with `golang.org/x/mod` explicitly bumped above every version
DependaProxy's CVE gate denies (`GO-2026-6179`/`GO-2026-6180`).

That explicit pin only resolves if the target version of `golang.org/x/mod`
is fetchable somewhere. DependaProxy denies it outright, so `hack/tools`'
build resolves it from a local, pre-fetched, DependaProxy-validated module
cache at `/workspace/gomodcache/cache/download` (this sandbox only) via a
`GOPROXY=file:///work/gomodcache/cache/download,http://dependaproxy:8080/goproxy`
chain — file cache first, DependaProxy second for everything else. The
`Makefile`'s `bin/controller-gen` target detects whether that cache exists
(`TOOLS_GOPROXY` in the `Makefile`) and only sets the file:// proxy when it
does; on a normal CI runner (no such path), it's unset and `hack/tools`
builds against the ordinary Go proxy chain with no special handling — see
the `api` job in `.github/workflows/operator.yml`.

If `/workspace/gomodcache/cache/download` is ever missing or lacks the
pinned `golang.org/x/mod` version, re-seed it by fetching
`golang.org/x/mod@<version>` through DependaProxy directly (it validates
and caches on first successful fetch) before running `make manifests` /
`make generate` in this sandbox.

`setup-envtest` (the usual way to fetch controller-gen *and* envtest
binaries together) is not used at all: both its released versions declare
`go 1.26.0`, unbuildable on the `golang:1.25` toolchain this project pins,
and both pin a denied `x/mod` version — the same dead end as `go install
controller-gen@version`, just one level removed.

### `make envtest-assets` / `make test-envtest`

Since `setup-envtest` is a dead end here, `hack/fetch-envtest.sh` downloads
the real `kube-apiserver`/`etcd`/`kubectl` binaries directly from
`controller-tools`' `envtest-v<version>` GitHub releases and verifies their
sha512 checksum before use — idempotent, and independent of `go install`
entirely. `internal/apitest/` is the envtest-backed test suite: it installs
both CRDs from `config/crd/bases/` into a real (locally started)
`kube-apiserver`/`etcd`, then round-trips fully-populated objects, checks
every documented defaulting/validation/immutability rule against the real
API server (not against controller code), and checks the printer columns
and short names/categories via discovery. It's skipped (not failed) when
`KUBEBUILDER_ASSETS` isn't set, so `make test` still passes without the
envtest binaries present.

`internal/controller/` mirrors the same `TestMain` skip-guard pattern for its
own envtest-backed suite: it exercises the real reconcile loop (`Reconcile`,
`writeStatus`'s conflict-retry and staleness guard, and the one
manager-based watch test) against a real `kube-apiserver`, with an injectable
clock and an injectable `ObserveFunc` so timeout- and facts-driven
transitions are deterministic without a real cluster clock or real
subsystems.
