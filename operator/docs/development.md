# Development

Building, testing, and the toolchain constraints of this repo. For what the
operator *does*, see [`../README.md`](../README.md); for the docs
themselves, see "Writing docs" below.

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
| `test` | `go test -race` with coverage (includes `internal/crdref` and `internal/docs`) |
| `vet` | `go vet ./...` |
| `fmt` / `fmt-check` | `gofmt -w` / drift check |
| `lint` | `golangci-lint run` |
| `vuln` | `govulncheck` |
| `tidy` | `go mod tidy` |
| `manifests` | CRD manifests (`config/crd/bases/*.yaml`) and RBAC (`config/rbac/role.yaml`) via `controller-gen`, built from `hack/tools` |
| `generate` | deepcopy generation (`api/v1alpha1/zz_generated.deepcopy.go`) via `controller-gen` |
| `crd-docs` | regenerate `docs/crd-reference.md` from `config/crd/bases/*.yaml` |
| `crd-docs-check` | fail if `docs/crd-reference.md` has drifted from `config/crd/bases/*.yaml` |
| `envtest-assets` | download and checksum-verify real kube-apiserver/etcd/kubectl binaries for envtest |
| `test-envtest` | `go test -race` against `internal/apitest` and `internal/controller`, using the envtest binaries above |
| `kustomize-build` | render `config/default` |
| `cross` | cross-compile linux/amd64 and linux/arm64 |
| `docker-build` | build the operator image |
| `smoke` | build then run `hack/smoke.sh` — starts the manager outside a cluster, checks `/healthz` and `/readyz`, and confirms clean shutdown on SIGTERM |
| `clean` | remove build artifacts |
| `minio-up` / `minio-down` | start/stop a real MinIO container (on a dedicated docker network) for `internal/storage`'s s3 conformance leg |
| `test-minio` | `go test -race` against `internal/storage` only, with `SANDBOX_TEST_MINIO_*` pointed at `minio-up`'s container |
| `storage-conformance` | `minio-up` -> `test-minio` -> `minio-down`, tearing down even on failure (same shape as `e2e`) |
| `e2e-tools` | fetch pinned `kind`/`kubectl`/`helm` binaries for the e2e suite |
| `e2e-up` / `e2e-down` | bring up / tear down the kind-based e2e cluster |
| `e2e-run` | run the ginkgo e2e suite against the cluster `e2e-up` created |
| `e2e` | `e2e-up` -> `e2e-run` -> `e2e-down`, tearing down even on failure (`E2E_KEEP=1` skips the final teardown) |
| `e2e-collect` | gather diagnostics (deployment describe/logs, CRs, events) from a live e2e cluster |
| `e2e-tidy` / `e2e-vet` | `go mod tidy` / `go vet` for the separate `test/e2e` and `test/e2e/doubles` modules |
| `helm-crds` | copy `config/crd/bases/*.yaml` into the chart's `crds/` directory |
| `helm-crds-check` | fail if the chart's `crds/` copy has drifted from `config/crd/bases` |
| `helm-rbac-check` | fail if the chart's `ClusterRole` rules have drifted from `config/rbac/role.yaml` |
| `helm-lint` / `helm-template` | lint / render the Helm chart |
| `helm-kind` | run `hack/helm-kind-walkthrough.sh` (install/upgrade/uninstall/reinstall assertions) against a Helm-deployed kind cluster |
| `quickstart-check` | run `hack/quickstart-check.sh`, which extracts and executes the `sh quickstart`-tagged fenced blocks from `../README.md` against a real kind cluster |

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
  `ginkgo`->`golang.org/x/tools`->`golang.org/x/mod`) — this repository
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
- `github.com/minio/minio-go/v7` — the "obvious" S3 client — is
  **unusable** here, not just dispreferred: it transitively imports
  `golang.org/x/crypto/argon2` via `pkg/encrypt`, and DependaProxy's CVE
  gate denies `golang.org/x/crypto` at every version tested
  (`GO-2026-5932`, an "unmaintained package" advisory with no fixed
  version). `internal/storage`'s S3 backend (#26) uses
  `github.com/aws/aws-sdk-go-v2/service/s3` instead, which resolves
  cleanly.
- The AWS SDK set must be added as a coherent group with one **pinned**
  command, not an unpinned `go get`/`go mod tidy`: an unpinned resolution
  re-resolves to `@latest`, which always 403s on DependaProxy's 7-day
  publication-age gate.
  ```
  go get github.com/aws/aws-sdk-go-v2/feature/s3/manager@v1.22.37 && go mod tidy -e
  ```
  This pulls in `aws-sdk-go-v2 v1.43.2`, `feature/s3/manager v1.22.37`,
  `service/s3 v1.106.2`, `credentials v1.19.32`, plus the internal
  protocol/endpoint packages those require — a coherent set that clears
  DependaProxy's gate. If a later `go mod tidy` re-resolves any of these
  to a newer, 403'd version, re-run the pinned `go get` for
  `feature/s3/manager@v1.22.37` again; do not loosen the pin to resolve a
  tidy conflict.
- `feature/s3/manager` itself carries an upstream deprecation notice
  ("superceded by feature/s3/transfermanager") — that replacement,
  `feature/s3/transfermanager`, is still pre-1.0 at the time of writing and
  was not used here; `manager.Uploader` is still the correct choice for a
  streaming multipart upload from a plain `io.Reader` (see
  `internal/storage/s3.go`'s `Put`). Its read-side counterpart,
  `manager.Downloader`, is deliberately NOT used — it requires an
  `io.WriterAt`, which would defeat streaming — `Get` uses plain
  `s3.GetObject` instead.
- `github.com/klauspost/compress` moved from a pinned **indirect** require
  (bumped only to clear controller-runtime/client-go's transitive CVE gate)
  to a **direct** require: `internal/storage/archive.go` now imports
  `github.com/klauspost/compress/zstd` directly, for the workspace/
  agent-home archive format. `go mod tidy` promotes it automatically; the
  version stays the same (`v1.18.7`).

## `make manifests` / `make generate`: `hack/tools`

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

## `make envtest-assets` / `make test-envtest`

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

## Writing docs

`docs/crd-reference.md` is **generated** — never hand-edit it. Edit the Go
doc comments and `+kubebuilder` markers on `api/v1alpha1/*.go` instead, run
`make manifests generate crd-docs`, and commit the result. CI
(`.github/workflows/operator.yml`, job `api`) fails the build if the
committed file differs from a fresh render.

The quickstart in `../README.md` is a **CI-executed documentation test**:
every fenced block tagged `` ```sh quickstart `` is extracted verbatim by
`hack/quickstart-check.sh` and run against a real kind cluster in
`.github/workflows/docs.yml`. Do not add a `quickstart`-tagged block that is
not idempotent, not machine-runnable, or that needs a credential this repo
cannot supply in CI. Use an untagged `` ```sh ``/`` ```console `` block for
anything illustrative — those are never extracted or executed.

## Running the doc tests locally

- `make IN_CONTAINER=1 crd-docs-check` — confirms `docs/crd-reference.md`
  matches the committed CRD YAML.
- `make IN_CONTAINER=1 test` — now also runs `internal/crdref` (the CRD-doc
  renderer's own tests) and `internal/docs` (`TestEveryConditionReasonIsDocumented`,
  which fails the build if a new condition reason is added to
  `lifecycle.AllReasons`/`controller.AllNetworkConditionReasons`/
  `controller.AllEngineSecurityReasons` without a matching row in
  `docs/operations.md`).
- `make IN_CONTAINER=1 quickstart-check` — runs the quickstart against a
  real kind cluster. Like `helm-kind`, this is only fully supported
  `IN_CONTAINER=1` (a CI runner, or any shell with `docker`/`kind`/
  `kubectl`/`helm` already on `PATH`) — the sandbox's nested-DinD wrapping
  cannot address a kind API server the way this script needs. Not required
  to pass locally; verify the extractor alone with `sh -n
  hack/quickstart-check.sh` and let CI run the rest.
