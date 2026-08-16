# ai-sandbox operator

Kubernetes operator for ai-sandbox. See [issue #15](https://github.com/psenna/ai-sandbox/issues/15)
for the broader design context.

This directory is currently a **scaffold only** (issue #16): a Go module,
kubebuilder v4 project layout, CI, and a manager that starts, serves health
probes, and exits cleanly. It has no API types or controllers yet — those
land in [#17](https://github.com/psenna/ai-sandbox/issues/17) (API types)
and [#18](https://github.com/psenna/ai-sandbox/issues/18) (controllers).

## Layout

```
operator/
├── cmd/main.go              entrypoint: config, manager, signal handling
├── internal/config/         flag/env configuration surface
├── internal/operator/       manager construction (scheme, options, probes)
├── config/                  kustomize bases (manager Deployment, RBAC)
├── hack/                    boilerplate header, smoke test script
├── Dockerfile                multi-stage, cross-compiled, distroless
└── Makefile                  build/test/lint entrypoints (see below)
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
| `manifests` | CRD/webhook manifests via `controller-gen` (no-op until #17 adds API types) |
| `generate` | deepcopy generation via `controller-gen` (no-op until #17 adds API types) |
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
  they can for our own module. As a result `make lint`, `make vuln`,
  `make manifests`, `make generate` and `make kustomize-build` cannot
  currently succeed in this environment — not a code or Makefile defect,
  but an upstream/proxy gap (no `golang.org/x/mod` release currently
  clears DependaProxy's CVE gate). These targets are expected to start
  working again once a patched `x/mod` release is available through
  DependaProxy; no workaround was applied since bypassing the CVE gate is
  out of scope here.
