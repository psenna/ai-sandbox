# k8s-native control API (services.yaml → ServiceSet, exec) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the agent a loopback control-API surface — `sandboxctl services apply` (POST a `services.yaml` declaration, the sidecar upserts a `ServiceSet` CR owned by the environment), `sandboxctl services compose` (render the equivalent `docker-compose.yml` from the same declaration), and `sandboxctl exec` (one-shot run a command inside a declared runtime pod) — plus admission validation, RBAC widening, a controller defense-in-depth guard for the deferred name-collision defect, and NetworkPolicy integration so dep/runtime pods inherit the environment's isolation.

**Architecture:** The agent process holds NO Kubernetes credential. The agent CLI subcommands (`services apply`, `services compose`, `exec`) are HTTP clients to the existing loopback control API at `127.0.0.1:9099` (`compose` is pure and never calls the server). The sandboxctl sidecar — which already holds the namespace-scoped ServiceAccount token — handles `POST /v1/services` (validate the declaration, upsert the `ServiceSet` CR named `<env>`, `EnvironmentName=<env>`, owned by the `SandboxEnvironment`) and `POST /v1/exec` (SPDY exec into the runtime pod, one-shot stdout/stderr/stdin). The `ServiceSetReconciler` (from Plan 1) reconciles the CR to Pods/Services/PVCs. A new `k8s-native` engine type is added as a no-op `Engine` (it needs no pod sidecar — deps/runtimes are separate pods), so existing render/RBAC seams branch on it. The deferred name-collision defect (#2) is fixed at two layers: admission validation rejects a name shared across `services`+`runtimes`, and the controller short-circuits a colliding `ServiceSet` to `Ready=False/DuplicateEntryName` (defense-in-depth for a hand-crafted CR that bypasses admission). Dep/runtime pods are labeled with the environment label and set `automountServiceAccountToken:false`, and the Restricted NetworkPolicy gains an egress rule letting the agent reach env-labeled pods — so dep/runtime pods inherit the namespace's isolation by construction.

**Tech Stack:** Go 1.22+ `http.NewServeMux` method patterns; `sigs.k8s.io/yaml` for `services.yaml`↔`docker-compose.yml`; `sigs.k8s.io/controller-runtime` `client.Client` for the ServiceSet upsert; `k8s.io/client-go/kubernetes` + `remotecommand` SPDY for exec; envtest (controller-runtime's `envtest`) for controller/server-store tests; `httptest` for handler/client tests.

## Global Constraints

Inherited from the approved design (`docs/superpowers/specs/2026-08-23-k8s-native-execution-model-design.md`) and Plan 1's codebase conventions — every task's requirements implicitly include these:

- **API group** `sandbox.psenna.dev/v1alpha1`. API types self-register via `func init(){ SchemeBuilder.Register(&T{}, &TList{}) }`.
- **Generated artifacts** (`make generate` for deepcopy, `make manifests` for CRD+RBAC, `make helm-crds`, `make crd-docs`) are committed; the CRD enum change in Task 1 requires a `make manifests` regeneration.
- **Local make always passes `IN_CONTAINER=1`** (a parse-time guard errors otherwise). Run every make target from `operator/`. With it, `DOCKER_RUN` is empty so make runs go/shell directly.
- **envtest** loads CRDs from `operator/config/crd/bases/`; it has NO kubelet/controller-manager, so Pods/PVCs stay `Pending` — tests assert object existence and simulate readiness via status patches. A status `Patch` bumps `resourceVersion`; re-fetch before a full-object `Update`. envtest assets at `/tmp/envtest/k8s/1.35.0`. Full suite: `KUBEBUILDER_ASSETS=/tmp/envtest/k8s/1.35.0 IN_CONTAINER=1 make test` (or `go test -race -count=1 ./...` under `IN_CONTAINER=1`).
- **The agent holds no Kubernetes credential.** `automountServiceAccountToken:false` on dep/runtime pods (Task 8); the sidecar's SA token is projected into the sandboxctl container ONLY, never the agent container. The agent CLI subcommands hold NO `client.Client` — they are HTTP clients to the loopback control API only. The sidecar (which already has the token) performs all k8s calls.
- **Control API is loopback-only.** `validateLoopbackListen` (config.go) rejects a non-loopback bind. The new client subcommands target `--listen` (default `127.0.0.1:9099`); a non-loopback `--listen` for a client is a config error (reuse `validateLoopbackListen`).
- **No hostPath/privileged/devices/`allowPrivilegeEscalation:true`** in the ServiceSet controller. The `k8s-native` engine contributes no `Relaxation` (it needs none), so `EngineSecurityRelaxed` is `False/NoRelaxation`.
- **Data PVCs are RWO; the workspace PVC is RWO single-node baseline** (RWX/multi-node deferred). Dep/runtime pods mount the shared workspace PVC `<envName>-workspace` by name (owned by the env controller, NOT by the ServiceSet controller — the controller references it by name only).
- **RBAC is scoped.** The sidecar Role is namespace-scoped and pinned by `resourceNames` where the name is fixed; `pods/exec` cannot be name-pinned (runtime pod names are dynamic) so it is namespace-scoped only — documented honestly like the existing per-subresource-not-per-field comment. No `list`/`watch` on servicesets (the sidecar upserts by name, never lists).
- **Diagnostics Secrets are projected by NAME + KEY-NAMES ONLY**, never decoded values.
- **DRY / YAGNI / TDD.** Failing test first, then minimal code. Interactive/streaming/TTY exec is deferred — `exec` is one-shot (command + optional stdin → stdout/stderr). The `k8s-native` engine does NOT change the default engine (default stays `rootless-podman`); k8s-native is opted into via the class engine.
- **The sandbox CANNOT run kind locally** (nested DinD). Controller/server-store logic is validated by envtest; full e2e is Plan 3 (CI). Do NOT add kind-based local e2e here.

---

## File Structure

**Create:**
- `operator/internal/render/engine_k8snative.go` — `k8sNativeEngine` (no-op `Engine`, mirrors `noneEngine`).
- `operator/internal/render/engine_k8snative_test.go` — Type/Contribute/EngineRelaxations assertions.
- `operator/internal/sandboxctl/services_yaml.go` — `ParseServicesYAML` + `Validate` (shared, incl. cross-list name collision — the #2 admission fix).
- `operator/internal/sandboxctl/services_yaml_test.go` — parse + validation table tests.
- `operator/internal/sandboxctl/compose.go` — `Compose` renders `docker-compose.yml` from a `ServiceSetSpec`.
- `operator/internal/sandboxctl/compose_test.go` — structural comparison against golden declarations.
- `operator/internal/sandboxctl/serviceset_store.go` — `serviceSetStore` upserts the ServiceSet CR (owned by the env, `EnvironmentName=env`).
- `operator/internal/sandboxctl/serviceset_store_test.go` — envtest upsert + owner ref + EnvironmentName.
- `operator/internal/sandboxctl/exec.go` — `Execer` interface + `podExecer` (SPDY one-shot).
- `operator/internal/sandboxctl/exec_test.go` — handler with a fake `Execer` (httptest).
- `operator/internal/sandboxctl/control_client.go` — `ControlClient` (loopback HTTP client for `/v1/services` + `/v1/exec`).
- `operator/internal/sandboxctl/control_client_test.go` — httptest round-trips.
- `operator/internal/sandboxctl/services_cli.go` — `RunServicesApply`, `RunServicesCompose`, `RunExec` (CLI entrypoints).
- `operator/internal/sandboxctl/services_cli_test.go` — CLI against an httptest server.

**Modify:**
- `operator/api/v1alpha1/sandboxclass_types.go` — add `EngineTypeK8sNative`; widen the `EngineSpec.Type` enum marker to `rootless-podman;none;k8s-native`.
- `operator/internal/render/engine.go` — register `k8sNativeEngine` in `engineRegistry`; rewrite the superseded "deliberately NOT a k8s-native stub" comment.
- `operator/internal/render/rbac.go` — `renderRole` widens for `k8s-native` (servicesets get/create/update; pods/exec create), gated on `in.Class.Spec.Engine.Type == k8s-native`.
- `operator/internal/render/rbac_test.go` — assert the new rules are present for k8s-native, absent for none.
- `operator/internal/sandboxctl/api.go` — `ServicesApplyRequest/Response`, `ExecRequest/Response`, new error codes.
- `operator/internal/sandboxctl/limits.go` — services/exec rate + body-size constants.
- `operator/internal/sandboxctl/server.go` — `NewServer` gains a `serviceSetApplier` + `Execer`; register `POST /v1/services` + `POST /v1/exec` (with `methodNotAllowed` bare registrations); buckets.
- `operator/internal/sandboxctl/handlers.go` — `handlers` gains `sets serviceSetApplier` + `execer Execer`; `handleServicesApply` + `handleExec`.
- `operator/internal/sandboxctl/run.go` — build `serviceSetStore` + `podExecer`; pass to `NewServer`.
- `operator/cmd/sandboxctl/main.go` — dispatch `services` (apply/compose) + `exec`.
- `operator/internal/controller/serviceset_controller.go` — defense-in-depth guard (Task 7); env label + `automountServiceAccountToken:false` (Task 8).
- `operator/internal/render/networkpolicy.go` — env egress rule (agent → env-labeled pods) (Task 8).
- `operator/config/crd/bases/` — regenerated by `make manifests` (Task 1).

**Reference (read-only context, do not modify):**
- `docs/superpowers/specs/2026-08-23-k8s-native-execution-model-design.md` — approved design (§1 field set, §2 reconcile, §3 exec, §4 security, §6 test surface).
- `docs/superpowers/plans/2026-08-23-k8s-native-serviceset.md` — Plan 1 (CRD + reconciler); the `ServiceSetReconciler` this plan extends.
- `operator/api/v1alpha1/serviceset_types.go` — `ServiceSetSpec`/`ServiceSpec`/`RuntimeSpec`/`HealthcheckSpec`/`ServiceStorageSpec` (the types the parser/compose/render operate on).

---

## Task 1: `EngineTypeK8sNative` + no-op `k8sNativeEngine` + registry

**Files:**
- Modify: `operator/api/v1alpha1/sandboxclass_types.go` (add const + widen enum marker)
- Create: `operator/internal/render/engine_k8snative.go`
- Test: `operator/internal/render/engine_k8snative_test.go`
- Modify: `operator/internal/render/engine.go` (registry + comment)

**Interfaces:**
- Consumes: `render.Engine` interface (`Type() v1alpha1.EngineType` + `Contribute(Inputs) (Contribution, error)`) from `engine.go`; `noneEngine` pattern from `engine_none.go`.
- Produces: `v1alpha1.EngineTypeK8sNative == "k8s-native"`; `k8sNativeEngine{}` registered in `engineRegistry` so `engineFor("k8s-native")` resolves and `EngineRelaxations("k8s-native")` returns `(nil, true)`. Tasks 4/8 branch on this type.

- [ ] **Step 1: Write the failing test**

`operator/internal/render/engine_k8snative_test.go`:
```go
package render

import (
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestK8sNativeEngineRegisteredAndNoop(t *testing.T) {
	e, err := engineFor(v1alpha1.EngineTypeK8sNative)
	if err != nil {
		t.Fatalf("engineFor(k8s-native): %v", err)
	}
	if e.Type() != v1alpha1.EngineTypeK8sNative {
		t.Fatalf("Type() = %q, want %q", e.Type(), v1alpha1.EngineTypeK8sNative)
	}
	c, err := e.Contribute(Inputs{})
	if err != nil {
		t.Fatalf("Contribute: %v", err)
	}
	if len(c.Containers) != 0 || len(c.Volumes) != 0 || len(c.Relaxations) != 0 {
		t.Fatalf("Contribute not a no-op: %+v", c)
	}
	// EngineRelaxations must report the engine as knowable AND relaxed=false.
	relaxations, ok := EngineRelaxations(v1alpha1.EngineTypeK8sNative)
	if !ok {
		t.Fatal("EngineRelaxations(k8s-native) ok=false, want true (engine is implemented)")
	}
	if len(relaxations) != 0 {
		t.Fatalf("EngineRelaxations(k8s-native) = %v, want empty (k8s-native needs no relaxations)", relaxations)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `IN_CONTAINER=1 go test ./internal/render/ -run TestK8sNativeEngineRegisteredAndNoop -v`
Expected: FAIL — `engineFor: unknown engine type "k8s-native"` (const + registry not yet added).

- [ ] **Step 3: Add the `EngineTypeK8sNative` constant + widen the enum**

In `operator/api/v1alpha1/sandboxclass_types.go`, add after `EngineTypeNone`:
```go
	// EngineTypeK8sNative runs declared dependency services and dev-tool
	// runtimes as native Kubernetes Pods/Services in the sandbox namespace
	// (reconciled by the ServiceSet CRD), instead of a nested container
	// runtime. The agent pod is a thin dispatcher; deps/runtimes are isolated
	// peers governed by the namespace's NetworkPolicy. See issue #24.
	EngineTypeK8sNative EngineType = "k8s-native"
```

Widen the `EngineSpec.Type` enum marker (the `+kubebuilder:validation:Enum=rootless-podman;none` line) to:
```go
	// +kubebuilder:validation:Enum=rootless-podman;none;k8s-native
	// +kubebuilder:default=rootless-podman
```
Leave the default as `rootless-podman` (k8s-native is opted into, not the default — see Global Constraints).

- [ ] **Step 4: Create `k8sNativeEngine`**

`operator/internal/render/engine_k8snative.go`:
```go
package render

import "github.com/psenna/ai-sandbox/operator/api/v1alpha1"

// k8sNativeEngine is the k8s-native engine (#24): dependencies and dev-tool
// runtimes are native Pods/Services declared via the ServiceSet CRD and
// reconciled by the ServiceSetReconciler, NOT a container nested in the agent
// pod. The engine therefore contributes nothing to the agent pod -- no
// sidecar, no volumes, no security relaxation. The agent container runs by
// itself, exactly like noneEngine; the distinction is the ServiceSet control
// model, surfaced through the sidecar's /v1/services and /v1/exec endpoints
// (see internal/sandboxctl) and the RBAC widened for this engine (rbac.go).
type k8sNativeEngine struct{}

func (k8sNativeEngine) Type() v1alpha1.EngineType { return v1alpha1.EngineTypeK8sNative }

func (k8sNativeEngine) Contribute(Inputs) (Contribution, error) { return Contribution{}, nil }
```

- [ ] **Step 5: Register it + rewrite the superseded comment**

In `operator/internal/render/engine.go`, add to `engineRegistry`:
```go
var engineRegistry = map[v1alpha1.EngineType]Engine{
	v1alpha1.EngineTypeNone:           noneEngine{},
	v1alpha1.EngineTypeRootlessPodman: notImplementedEngine{typ: v1alpha1.EngineTypeRootlessPodman, issue: 24},
	v1alpha1.EngineTypeK8sNative:      k8sNativeEngine{},
}
```
Rewrite the `notImplementedEngine` doc comment (the one beginning "This is deliberately NOT a k8s-native stub") to reflect that k8s-native is now implemented:
```go
// notImplementedEngine is registered (so the dispatch/seam machinery is real)
// but always fails closed, naming the issue that ships the real implementation.
// rootless-podman remains the unimplemented stub: issue #24 was re-scoped to a
// k8s-native engine (see engine_k8snative.go + the approved design), so the
// rootless-podman-in-a-pod approach is abandoned and its branch excluded.
```

- [ ] **Step 6: Regenerate CRD manifests + run tests**

Run:
```bash
IN_CONTAINER=1 make manifests
IN_CONTAINER=1 make helm-crds
IN_CONTAINER=1 go test ./internal/render/ -run TestK8sNative -v
```
Expected: PASS. Verify the regenerated `operator/config/crd/bases/sandbox.psenna.dev_sandboxclasses.yaml` `engine.type` enum now lists `k8s-native` (grep it). Commit the regenerated CRD.

- [ ] **Step 7: Commit**

```bash
git add api/v1alpha1/sandboxclass_types.go internal/render/engine_k8snative.go internal/render/engine_k8snative_test.go internal/render/engine.go config/crd/bases/ charts/
git commit -m "feat(render): add k8s-native engine type (no-op, needs no pod sidecar)

Refs #24 (Plan 2 of 3; does not close)"
```

---

## Task 2: `services.yaml` parser + shared `Validate` (incl. cross-list name collision — #2 admission fix)

The declaration reuses the Plan 1 API types directly: `services.yaml` unmarshals into a `{Services, Runtimes}` struct carrying `v1alpha1.ServiceSpec`/`v1alpha1.RuntimeSpec` (which carry `json` tags), so `sigs.k8s.io/yaml` handles YAML↔JSON with no separate declaration schema and no field-name drift. `Validate` runs on a full `ServiceSetSpec` and is shared by the CLI (client-side pre-check), the server handler (defense-in-depth), and exercised by compose's test fixtures.

**Files:**
- Create: `operator/internal/sandboxctl/services_yaml.go`
- Test: `operator/internal/sandboxctl/services_yaml_test.go`

**Interfaces:**
- Consumes: `v1alpha1.ServiceSetSpec`/`ServiceSpec`/`RuntimeSpec`/`HealthcheckSpec`/`ServiceStorageSpec` from `operator/api/v1alpha1/serviceset_types.go`; `sigs.k8s.io/yaml` (already a transitive k8s dependency — confirm with `go list -m sigs.k8s.io/yaml` in `operator/`; it is present via controller-runtime).
- Produces:
  - `func ParseServicesYAML(data []byte) (v1alpha1.ServiceSetSpec, error)` — unmarshal; `EnvironmentName` left empty (the server sets it).
  - `func ValidateServiceSet(spec v1alpha1.ServiceSetSpec) error` — returns a `*ValidationError` (first failure) or nil. Codes: `CodeMissingParam`, `CodeDuplicateEntryName`, `CodeDanglingDependsOn`, `CodeInvalidDeclaration`.
  - `type ValidationError struct { Code, Message, Field string }` with `Error() string`, used by Task 5's handler via the existing `writeValidationError`.

- [ ] **Step 1: Write the failing test**

`operator/internal/sandboxctl/services_yaml_test.go`:
```go
package sandboxctl

import (
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

const validYAML = `
services:
  - name: postgres
    image: postgres:18-alpine
    ports: [5432]
    env:
      POSTGRES_USER: e2e
    storage:
      size: 1Gi
      mountPath: /var/lib/postgresql/data
    healthcheck:
      exec: ["pg_isready", "-U", "e2e"]
      interval: 5s
    dependsOn: []
runtimes:
  - name: python
    image: python:3.13-slim
    mountWorkspace: true
    command: ["sleep", "infinity"]
`

func TestParseServicesYAML(t *testing.T) {
	spec, err := ParseServicesYAML([]byte(validYAML))
	if err != nil {
		t.Fatalf("ParseServicesYAML: %v", err)
	}
	if len(spec.Services) != 1 || spec.Services[0].Name != "postgres" {
		t.Fatalf("services = %+v", spec.Services)
	}
	if len(spec.Runtimes) != 1 || spec.Runtimes[0].Name != "python" {
		t.Fatalf("runtimes = %+v", spec.Runtimes)
	}
	if spec.Services[0].Storage == nil || spec.Services[0].Storage.Size != "1Gi" {
		t.Fatalf("storage not parsed: %+v", spec.Services[0].Storage)
	}
	if spec.Services[0].Healthcheck.Exec[0] != "pg_isready" {
		t.Fatalf("healthcheck not parsed: %+v", spec.Services[0].Healthcheck)
	}
	if spec.EnvironmentName != "" {
		t.Fatalf("EnvironmentName should be empty (server sets it), got %q", spec.EnvironmentName)
	}
}

func TestValidateServiceSet(t *testing.T) {
	cases := []struct {
		name    string
		spec    v1alpha1.ServiceSetSpec
		wantCode string
	}{
		{
			name: "valid",
			spec: v1alpha1.ServiceSetSpec{
				EnvironmentName: "env-1",
				Services: []v1alpha1.ServiceSpec{{Name: "postgres", Image: "postgres:18"}},
				Runtimes: []v1alpha1.RuntimeSpec{{Name: "python", Image: "python:3.13"}},
			},
			wantCode: "",
		},
		{
			name: "missing name",
			spec: v1alpha1.ServiceSetSpec{EnvironmentName: "e", Services: []v1alpha1.ServiceSpec{{Image: "x"}}},
			wantCode: CodeMissingParam,
		},
		{
			name: "missing image",
			spec: v1alpha1.ServiceSetSpec{EnvironmentName: "e", Runtimes: []v1alpha1.RuntimeSpec{{Name: "r"}}},
			wantCode: CodeMissingParam,
		},
		{
			name: "cross-list name collision (#2)",
			spec: v1alpha1.ServiceSetSpec{EnvironmentName: "e",
				Services:  []v1alpha1.ServiceSpec{{Name: "shared", Image: "a"}},
				Runtimes:  []v1alpha1.RuntimeSpec{{Name: "shared", Image: "b"}}},
			wantCode: CodeDuplicateEntryName,
		},
		{
			name: "within-list duplicate",
			spec: v1alpha1.ServiceSetSpec{EnvironmentName: "e",
				Services: []v1alpha1.ServiceSpec{{Name: "x", Image: "a"}, {Name: "x", Image: "b"}}},
			wantCode: CodeDuplicateEntryName,
		},
		{
			name: "dangling dependsOn",
			spec: v1alpha1.ServiceSetSpec{EnvironmentName: "e",
				Services: []v1alpha1.ServiceSpec{{Name: "a", Image: "x", DependsOn: []string{"nope"}}}},
			wantCode: CodeDanglingDependsOn,
		},
		{
			name: "storage missing size",
			spec: v1alpha1.ServiceSetSpec{EnvironmentName: "e",
				Services: []v1alpha1.ServiceSpec{{Name: "a", Image: "x", Storage: &v1alpha1.ServiceStorageSpec{MountPath: "/d"}}}},
			wantCode: CodeInvalidDeclaration,
		},
		{
			name: "healthcheck two probes",
			spec: v1alpha1.ServiceSetSpec{EnvironmentName: "e",
				Services: []v1alpha1.ServiceSpec{{Name: "a", Image: "x",
					Healthcheck: v1alpha1.HealthcheckSpec{Exec: []string{"true"}, TCP: &v1alpha1.TCPProbe{Port: 1}}}}},
			wantCode: CodeInvalidDeclaration,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateServiceSet(c.spec)
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want code %q, got nil", c.wantCode)
			}
			ve, ok := err.(*ValidationError)
			if !ok {
				t.Fatalf("error not a *ValidationError: %T %v", err, err)
			}
			if ve.Code != c.wantCode {
				t.Fatalf("code = %q, want %q", ve.Code, c.wantCode)
			}
			if !strings.Contains(ve.Message, c.name) && !strings.Contains(ve.Message, "shared") && !strings.Contains(ve.Message, "x") && !strings.Contains(ve.Message, "a") {
				// Message must name the offending entry/value (spot-check it is non-empty).
				if ve.Message == "" {
					t.Fatal("empty message")
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `IN_CONTAINER=1 go test ./internal/sandboxctl/ -run 'TestParseServicesYAML|TestValidateServiceSet' -v`
Expected: FAIL — `ParseServicesYAML` undefined, `ValidateServiceSet` undefined, codes undefined.

- [ ] **Step 3: Add the error codes**

In `operator/internal/sandboxctl/api.go`, append to the `const (...)` error-code block:
```go
	CodeDuplicateEntryName   = "duplicate_entry_name"
	CodeDanglingDependsOn    = "dangling_depends_on"
	CodeInvalidDeclaration   = "invalid_declaration"
	CodeServiceSetUpsertFailed = "serviceset_upsert_failed"
	CodeExecFailed           = "exec_failed"
```

- [ ] **Step 4: Implement `services_yaml.go`**

`operator/internal/sandboxctl/services_yaml.go`:
```go
package sandboxctl

import (
	"fmt"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// ValidationError is a structured, actionable validation failure returned by
// ValidateServiceSet. The handlers surface it verbatim via writeValidationError
// (the same path the wait/done handlers use).
type ValidationError struct {
	Code    string
	Message string
	Field   string
}

func (e *ValidationError) Error() string { return e.Message }

func validationError(code, msg, field string) *ValidationError {
	return &ValidationError{Code: code, Message: msg, Field: field}
}

// declaration is the services.yaml wire shape: services + runtimes only. The
// per-entry types ARE the Plan 1 API types (carrying json tags), so
// sigs.k8s.io/yaml parses the camelCase YAML fields with zero drift from the
// CRD schema. EnvironmentName is intentionally absent here -- the server sets
// it from its own identity before upserting the ServiceSet CR.
type declaration struct {
	Services []v1alpha1.ServiceSpec `json:"services,omitempty"`
	Runtimes []v1alpha1.RuntimeSpec `json:"runtimes,omitempty"`
}

// ParseServicesYAML unmarshals a services.yaml document into a ServiceSetSpec
// with EnvironmentName left empty. Callers (the CLI and the server) set
// EnvironmentName and then call ValidateServiceSet.
func ParseServicesYAML(data []byte) (v1alpha1.ServiceSetSpec, error) {
	var decl declaration
	if err := sigsyaml.Unmarshal(data, &decl); err != nil {
		return v1alpha1.ServiceSetSpec{}, fmt.Errorf("parsing services.yaml: %w", err)
	}
	return v1alpha1.ServiceSetSpec{Services: decl.Services, Runtimes: decl.Runtimes}, nil
}

// ValidateServiceSet returns the first validation failure as a *ValidationError,
// or nil if spec is well-formed. It is the single source of truth shared by the
// apply client (pre-check) and the server handler (defense-in-depth: the agent
// could POST with raw curl, bypassing the CLI). Checks:
//   - EnvironmentName non-empty (the server always sets it; a client-built
//     spec with it empty is a programming error, caught here).
//   - every entry has a non-empty Name and Image.
//   - no name is repeated, whether within one list or across services+runtimes
//     (the #2 defect: the CRD's +listMapKey=name pins uniqueness WITHIN each
//     list only; a service and runtime sharing a name is the storm the
//     reconciler would hit -- rejected here, and guarded in the controller).
//   - every dependsOn reference resolves to an existing entry name.
//   - storage, when set, has a non-empty Size and MountPath.
//   - a healthcheck, when it declares a probe, declares exactly one of
//     exec/http/tcp.
func ValidateServiceSet(spec v1alpha1.ServiceSetSpec) error {
	if spec.EnvironmentName == "" {
		return validationError(CodeMissingParam, "environmentName must not be empty", "environmentName")
	}
	names := map[string]string{} // name -> kind, to detect cross-list collisions
	all := map[string]struct{}{}

	checkEntry := func(name, image, kind string) *ValidationError {
		if name == "" {
			return validationError(CodeMissingParam, kind+" entry is missing a name", "name")
		}
		if image == "" {
			return validationError(CodeMissingParam, kind+" entry "+name+" is missing an image", "image")
		}
		if prev, ok := names[name]; ok {
			return validationError(CodeDuplicateEntryName,
				fmt.Sprintf("name %q appears in both %s and %s; a service and runtime cannot share a name", name, prev, kind), "name")
		}
		names[name] = kind
		all[name] = struct{}{}
		return nil
	}

	for i := range spec.Services {
		s := &spec.Services[i]
		if ve := checkEntry(s.Name, s.Image, "service"); ve != nil {
			return ve
		}
		if s.Storage != nil {
			if s.Storage.Size == "" || s.Storage.MountPath == "" {
				return validationError(CodeInvalidDeclaration, "service "+s.Name+" storage requires both size and mountPath", "storage")
			}
		}
		if ve := validateHealthcheck(s.Healthcheck, "service "+s.Name); ve != nil {
			return ve
		}
	}
	for i := range spec.Runtimes {
		rt := &spec.Runtimes[i]
		if ve := checkEntry(rt.Name, rt.Image, "runtime"); ve != nil {
			return ve
		}
		if ve := validateHealthcheck(rt.Healthcheck, "runtime "+rt.Name); ve != nil {
			return ve
		}
	}
	for _, s := range spec.Services {
		for _, dep := range s.DependsOn {
			if _, ok := all[dep]; !ok {
				return validationError(CodeDanglingDependsOn, "service "+s.Name+" dependsOn unknown entry "+dep, "dependsOn")
			}
		}
	}
	for _, rt := range spec.Runtimes {
		for _, dep := range rt.DependsOn {
			if _, ok := all[dep]; !ok {
				return validationError(CodeDanglingDependsOn, "runtime "+rt.Name+" dependsOn unknown entry "+dep, "dependsOn")
			}
		}
	}
	return nil
}

func validateHealthcheck(hc v1alpha1.HealthcheckSpec, who string) *ValidationError {
	n := 0
	if len(hc.Exec) > 0 {
		n++
	}
	if hc.HTTP != nil {
		n++
	}
	if hc.TCP != nil {
		n++
	}
	if n > 1 {
		return validationError(CodeInvalidDeclaration, who+" healthcheck must set at most one of exec/http/tcp", "healthcheck")
	}
	return nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `IN_CONTAINER=1 go test ./internal/sandboxctl/ -run 'TestParseServicesYAML|TestValidateServiceSet' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/sandboxctl/services_yaml.go internal/sandboxctl/services_yaml_test.go internal/sandboxctl/api.go
git commit -m "feat(sandboxctl): services.yaml parser + shared Validate (cross-list name collision, #2)

Refs #24 (Plan 2 of 3; does not close)"
```

---

## Task 3: `sandboxctl services compose` — render `docker-compose.yml` from the declaration

Pure client-side render. No server call, no k8s client. The CLI subcommand reads `services.yaml`, parses it (Task 2), and writes a structurally-equivalent `docker-compose.yml` to stdout (or `-o`). The test parses the rendered YAML and compares structure (robust to YAML string-vs-int quoting), not bytes.

**Files:**
- Create: `operator/internal/sandboxctl/compose.go`
- Test: `operator/internal/sandboxctl/compose_test.go`

**Interfaces:**
- Consumes: `ParseServicesYAML` (Task 2); `v1alpha1.ServiceSetSpec`/`ServiceSpec`/`RuntimeSpec`; `sigs.k8s.io/yaml.Marshal`.
- Produces: `func Compose(spec v1alpha1.ServiceSetSpec) ([]byte, error)` — marshals `{services: {...}, volumes: {...}}`. `sigs.k8s.io/yaml` uses yaml.v2 under the hood, which sorts map keys, so two renders of the same spec are byte-identical. Compose maps:
  - `image` → `image`; `command`→`entrypoint`, `args`→`command`; `env`→`environment`; `runAsUser`→`user` (string); `dependsOn`→`depends_on`; `restart: always` (implicit; the API has no Restart field).
  - service `ports`→`ports` as `["port:port"]`; `expose` (first port) → `["<expose>:<firstport>", "<rest>:<rest>..."]`; `storage`→`volumes: ["<name>-data:<mountPath>"]` + top-level `volumes.<name>-data: {}`.
  - runtime `mountWorkspace` (default true) → `volumes: ["workspace:/workspace"]` + top-level `volumes.workspace: {}`; default `command: [sleep, infinity]` when omitted.
  - `healthcheck.exec` → `{test: ["CMD", <exec...>], interval}`; `http`/`tcp` have no compose equivalent → omitted (documented limitation; compose only supports `test`).
  - `envFromSecret` has no compose equivalent (compose `env_file` references a file, not a k8s Secret) → omitted, documented.

- [ ] **Step 1: Write the failing test**

`operator/internal/sandboxctl/compose_test.go`:
```go
package sandboxctl

import (
	"testing"

	sigsyaml "sigs.k8s.io/yaml"
	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestCompose(t *testing.T) {
	spec := v1alpha1.ServiceSetSpec{
		Services: []v1alpha1.ServiceSpec{{
			Name: "postgres", Image: "postgres:18-alpine",
			Ports: []int32{5432},
			Env:  map[string]string{"POSTGRES_USER": "e2e"},
			Storage: &v1alpha1.ServiceStorageSpec{Size: "1Gi", MountPath: "/var/lib/postgresql/data"},
			Healthcheck: v1alpha1.HealthcheckSpec{Exec: []string{"pg_isready", "-U", "e2e"}, Interval: "5s"},
		}},
		Runtimes: []v1alpha1.RuntimeSpec{{Name: "python", Image: "python:3.13-slim", MountWorkspace: ptr(true)}},
	}
	out, err := Compose(spec)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	var doc map[string]any
	if err := sigsyaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rendered compose is not valid YAML: %v\n%s", err, out)
	}
	services := doc["services"].(map[string]any)

	pg := services["postgres"].(map[string]any)
	if pg["image"] != "postgres:18-alpine" {
		t.Fatalf("postgres image = %v", pg["image"])
	}
	if pg["restart"] != "always" {
		t.Fatalf("postgres restart = %v", pg["restart"])
	}
	ports := pg["ports"].([]any)
	if len(ports) != 1 || ports[0] != "5432:5432" {
		t.Fatalf("postgres ports = %v", ports)
	}
	env := pg["environment"].(map[string]any)
	if env["POSTGRES_USER"] != "e2e" {
		t.Fatalf("postgres env = %v", env)
	}
	vols := pg["volumes"].([]any)
	if vols[0] != "postgres-data:/var/lib/postgresql/data" {
		t.Fatalf("postgres volumes = %v", vols)
	}
	hc := pg["healthcheck"].(map[string]any)
	test := hc["test"].([]any)
	if test[0] != "CMD" || test[1] != "pg_isready" {
		t.Fatalf("postgres healthcheck.test = %v", test)
	}
	if hc["interval"] != "5s" {
		t.Fatalf("postgres healthcheck.interval = %v", hc["interval"])
	}

	py := services["python"].(map[string]any)
	if py["image"] != "python:3.13-slim" {
		t.Fatalf("python image = %v", py["image"])
	}
	cmd := py["command"].([]any)
	if cmd[0] != "sleep" || cmd[1] != "infinity" {
		t.Fatalf("python command = %v (default sleep infinity expected)", cmd)
	}
	pyVols := py["volumes"].([]any)
	if pyVols[0] != "workspace:/workspace" {
		t.Fatalf("python volumes = %v", pyVols)
	}

	volumes := doc["volumes"].(map[string]any)
	if _, ok := volumes["postgres-data"]; !ok {
		t.Fatalf("top-level volumes missing postgres-data: %v", volumes)
	}
	if _, ok := volumes["workspace"]; !ok {
		t.Fatalf("top-level volumes missing workspace: %v", volumes)
	}
}

func TestComposeExposeAndDeterminism(t *testing.T) {
	spec := v1alpha1.ServiceSetSpec{
		Services: []v1alpha1.ServiceSpec{{
			Name: "web", Image: "nginx", Ports: []int32{80, 81}, Expose: ptr[int32](18080),
		}},
	}
	out1, err := Compose(spec)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	out2, _ := Compose(spec)
	if string(out1) != string(out2) {
		t.Fatalf("Compose not deterministic:\n%s\n---\n%s", out1, out2)
	}
	var doc map[string]any
	_ = sigsyaml.Unmarshal(out1, &doc)
	ports := doc["services"].(map[string]any)["web"].(map[string]any)["ports"].([]any)
	if ports[0] != "18080:80" || ports[1] != "81:81" {
		t.Fatalf("expose ports = %v (want [18080:80, 81:81])", ports)
	}
}

// ptr is a tiny test helper for literals.
func ptr[T any](v T) *T { return &v }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `IN_CONTAINER=1 go test ./internal/sandboxctl/ -run TestCompose -v`
Expected: FAIL — `Compose` undefined.

- [ ] **Step 3: Implement `compose.go`**

`operator/internal/sandboxctl/compose.go`:
```go
package sandboxctl

import (
	"strconv"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// Compose renders the docker-compose.yml equivalent of a ServiceSetSpec.
// Pure and deterministic: sigs.k8s.io/yaml marshals via yaml.v2, which sorts
// map keys, so two renders of the same spec are byte-identical.
//
// Mapping notes (compose-aligned, per the approved design §1):
//   - command -> entrypoint, args -> command (compose semantics).
//   - restart is always "always" (the API has no Restart field; long-lived).
//   - envFromSecret has no compose equivalent (env_file references a file, not
//     a k8s Secret) -> omitted.
//   - healthcheck.http/.tcp have no compose equivalent (compose only supports
//     `test`) -> omitted; only healthcheck.exec translates.
//   - service storage -> a named volume "<name>-data"; runtime mountWorkspace
//     (default true) -> the shared named volume "workspace".
func Compose(spec v1alpha1.ServiceSetSpec) ([]byte, error) {
	services := map[string]any{}
	volumes := map[string]struct{}{}

	for _, s := range spec.Services {
		svc := map[string]any{"image": s.Image, "restart": "always"}
		if len(s.Ports) > 0 {
			svc["ports"] = composePorts(s.Ports, s.Expose)
		}
		if len(s.Env) > 0 {
			svc["environment"] = s.Env
		}
		if s.Storage != nil {
			svc["volumes"] = []string{s.Name + "-data:" + s.Storage.MountPath}
			volumes[s.Name+"-data"] = struct{}{}
		}
		if hc := composeHealthcheck(s.Healthcheck); hc != nil {
			svc["healthcheck"] = hc
		}
		if len(s.DependsOn) > 0 {
			svc["depends_on"] = s.DependsOn
		}
		if len(s.Command) > 0 {
			svc["entrypoint"] = s.Command
		}
		if len(s.Args) > 0 {
			svc["command"] = s.Args
		}
		if s.RunAsUser != nil {
			svc["user"] = strconv.FormatInt(*s.RunAsUser, 10)
		}
		services[s.Name] = svc
	}

	for _, rt := range spec.Runtimes {
		svc := map[string]any{"image": rt.Image, "restart": "always"}
		mount := true
		if rt.MountWorkspace != nil {
			mount = *rt.MountWorkspace
		}
		if mount {
			svc["volumes"] = []string{"workspace:/workspace"}
			volumes["workspace"] = struct{}{}
		}
		cmd := rt.Command
		if len(cmd) == 0 {
			cmd = []string{"sleep", "infinity"}
		}
		svc["command"] = cmd
		if len(rt.Args) > 0 {
			svc["entrypoint"] = rt.Command // keep entrypoint if both set
			svc["command"] = append(append([]string{}, rt.Command...), rt.Args...)
			if len(rt.Command) == 0 {
				svc["entrypoint"] = nil
				delete(svc, "entrypoint")
				svc["command"] = rt.Args
			}
		}
		if len(rt.Env) > 0 {
			svc["environment"] = rt.Env
		}
		if hc := composeHealthcheck(rt.Healthcheck); hc != nil {
			svc["healthcheck"] = hc
		}
		if len(rt.DependsOn) > 0 {
			svc["depends_on"] = rt.DependsOn
		}
		if rt.RunAsUser != nil {
			svc["user"] = strconv.FormatInt(*rt.RunAsUser, 10)
		}
		services[rt.Name] = svc
	}

	out := map[string]any{"services": services}
	if len(volumes) > 0 {
		// Convert struct{} values to empty maps so compose reads them as
		// `name: {}` (a valid, default-driver volume declaration).
		volMap := map[string]any{}
		for k := range volumes {
			volMap[k] = map[string]any{}
		}
		out["volumes"] = volMap
	}
	return sigsyaml.Marshal(out)
}

func composePorts(ports []int32, expose *int32) []string {
	out := make([]string, 0, len(ports))
	for i, p := range ports {
		if expose != nil && i == 0 {
			out = append(out, strconv.FormatInt(int64(*expose), 10)+":"+strconv.FormatInt(int64(p), 10))
			continue
		}
		out = append(out, strconv.FormatInt(int64(p), 10)+":"+strconv.FormatInt(int64(p), 10))
	}
	return out
}

func composeHealthcheck(hc v1alpha1.HealthcheckSpec) map[string]any {
	if len(hc.Exec) == 0 {
		return nil // http/tcp have no compose equivalent; omit.
	}
	m := map[string]any{"test": append([]string{"CMD"}, hc.Exec...)}
	if hc.Interval != "" {
		m["interval"] = hc.Interval
	}
	return m
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `IN_CONTAINER=1 go test ./internal/sandboxctl/ -run TestCompose -v`
Expected: PASS. (If the runtime `command`/`entrypoint` block above produces a wrong shape for the `python` case — `Command` is empty so the `len(rt.Args)==0` branch leaves `command = [sleep, infinity]` and no `entrypoint` — verify the test's `cmd[0]=="sleep"` holds. Adjust the runtime block if needed so an empty-Command runtime yields `command: [sleep, infinity]` and no `entrypoint`.)

- [ ] **Step 5: Commit**

```bash
git add internal/sandboxctl/compose.go internal/sandboxctl/compose_test.go
git commit -m "feat(sandboxctl): services compose renders docker-compose.yml from the declaration

Refs #24 (Plan 2 of 3; does not close)"
```

---

## Task 4: Sidecar RBAC widening (servicesets + pods/exec), gated on k8s-native

The sidecar Role widens ONLY when the class engine is `k8s-native`: `servicesets` get/create/update (pinned by `resourceNames=[env.Name]`, since the ServiceSet is named after the env) and `pods/exec` create (namespace-scoped only — runtime pod names are dynamic and cannot be name-pinned; documented honestly). The env `get` the store needs is already granted. The test asserts presence for k8s-native and absence for none.

**Files:**
- Modify: `operator/internal/render/rbac.go` (`renderRole`)
- Modify: `operator/internal/render/rbac_test.go` (assert new rules)

**Interfaces:**
- Consumes: `render.Inputs` (`in.Class.Spec.Engine.Type`, `in.Env.Name`, `in.Env.Namespace`); `acrbacv1.PolicyRule()`; `v1alpha1.EngineTypeK8sNative` (Task 1).
- Produces: `renderRole` returns a Role with 2 extra rules (servicesets get/create/update; pods/exec create) when `in.Class.Spec.Engine.Type == k1s-native`. Task 5's store relies on servicesets; Task 6's execer relies on pods/exec.

- [ ] **Step 1: Read the existing rbac_test.go**

Run: `cat operator/internal/render/rbac_test.go` (read the test harness — it asserts against a real authorizer; mirror its style for the new rules). Note: it builds an `Inputs` with a `Class`. You will add a k8s-native `Class` case and assert the authorizer permits servicesets get/create/update and pods/exec create for k8s-native, and denies them for `none`.

- [ ] **Step 2: Write the failing test additions**

Add to `rbac_test.go` a test `TestRenderRoleK8sNativeWide` (or extend the existing role test) that:
1. Builds `Inputs` with `Class.Spec.Engine.Type = v1alpha1.EngineTypeK8sNative`.
2. Calls `renderRole(in)`, walks `in.Rules`, and asserts there is a rule with `APIGroups=["sandbox.psenna.dev"]`, `Resources=["servicesets"]`, `ResourceNames=[env.Name]`, `Verbs` including get/create/update, AND a rule with `APIGroups=[""]`, `Resources=["pods/exec"]`, `Verbs=["create"]`.
3. Builds a second `Inputs` with `Class.Spec.Engine.Type = v1alpha1.EngineTypeNone` and asserts NO rule references `servicesets` or `pods/exec`.

Use the real authorizer if the existing test does (mirror exactly); otherwise assert on the returned `*RoleApplyConfiguration` rules directly.

- [ ] **Step 3: Run test to verify it fails**

Run: `IN_CONTAINER=1 go test ./internal/render/ -run TestRenderRole -v`
Expected: FAIL — no servicesets/pods/exec rules.

- [ ] **Step 4: Widen `renderRole`**

In `operator/internal/render/rbac.go`, change `renderRole` to append the k8s-native rules conditionally. Replace the `WithRules(...)` block:
```go
func renderRole(in Inputs) *acrbacv1.RoleApplyConfiguration {
	names := ChildNames(in.Env.Name)
	rules := []*acrbacv1.PolicyRuleApplyConfiguration{
		acrbacv1.PolicyRule().
			WithAPIGroups("sandbox.psenna.dev").
			WithResources("sandboxenvironments").
			WithResourceNames(in.Env.Name).
			WithVerbs("get"),
		acrbacv1.PolicyRule().
			WithAPIGroups("sandbox.psenna.dev").
			WithResources("sandboxenvironments/status").
			WithResourceNames(in.Env.Name).
			WithVerbs("get", "patch"),
	}
	if in.Class != nil && in.Class.Spec.Engine.Type == v1alpha1.EngineTypeK8sNative {
		// k8s-native adds the ServiceSet control model: the sidecar upserts one
		// ServiceSet CR named after the environment (resourceNames pins it -- the
		// CR name == env.Name), and one-shot execs into declared runtime pods
		// (POST /v1/exec -> SPDY pods/exec). pods/exec cannot be name-pinned:
		// runtime pod names are dynamic (declared in services.yaml at runtime),
		// so it is namespace-scoped only -- the same per-subresource-not-per-field
		// honesty as the status rule above. No list/watch on servicesets: the
		// sidecar upserts by name, never lists.
		rules = append(rules,
			acrbacv1.PolicyRule().
				WithAPIGroups("sandbox.psenna.dev").
				WithResources("servicesets").
				WithResourceNames(in.Env.Name).
				WithVerbs("get", "create", "update"),
			acrbacv1.PolicyRule().
				WithAPIGroups("").
				WithResources("pods/exec").
				WithVerbs("create"),
		)
	}
	return acrbacv1.Role(names.Role, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithRules(rules...)
}
```
Add the import `"github.com/psenna/ai-sandbox/operator/api/v1alpha1"` if not already present in rbac.go.

- [ ] **Step 5: Run test to verify it passes**

Run: `IN_CONTAINER=1 go test ./internal/render/ -run TestRenderRole -v`
Expected: PASS.

- [ ] **Step 6: Regenerate RBAC manifests + commit**

Run: `IN_CONTAINER=1 make manifests` (regenerates the sidecar Role ClusterRole/RoleBinding artifacts if controller-gen emits them — confirm `git diff --stat`). Then:
```bash
git add internal/render/rbac.go internal/render/rbac_test.go config/rbac/ 2>/dev/null || git add internal/render/rbac.go internal/render/rbac_test.go
git commit -m "feat(render): widen sidecar RBAC for k8s-native (servicesets + pods/exec)

Refs #24 (Plan 2 of 3; does not close)"
```

---

## Task 5: `sandboxctl services apply` — CLI client + `POST /v1/services` + `serviceSetStore` upsert

The CLI reads `services.yaml`, parses + validates it client-side, and POSTs the declaration to `POST /v1/services`. The server handler re-validates (defense-in-depth), sets `EnvironmentName = env.Name`, and the `serviceSetStore` upserts the ServiceSet CR (named `<env>`, owned by the env, `EnvironmentName=<env>`). The store Gets the env to fetch its UID for the OwnerReference (the env `get` is already granted by the sidecar Role). API errors map to 502 (matching `handleWait`); validation errors to 422.

**Files:**
- Create: `operator/internal/sandboxctl/serviceset_store.go`
- Create: `operator/internal/sandboxctl/serviceset_store_test.go` (envtest)
- Create: `operator/internal/sandboxctl/control_client.go`
- Create: `operator/internal/sandboxctl/control_client_test.go` (httptest)
- Create: `operator/internal/sandboxctl/services_cli.go`
- Create: `operator/internal/sandboxctl/services_cli_test.go` (httptest)
- Modify: `operator/internal/sandboxctl/api.go` (request/response types — error codes already added in Task 2)
- Modify: `operator/internal/sandboxctl/limits.go` (services bucket + body size)
- Modify: `operator/internal/sandboxctl/server.go` (`NewServer` signature + route registration)
- Modify: `operator/internal/sandboxctl/handlers.go` (`handlers.sets` field + `handleServicesApply`)
- Modify: `operator/internal/sandboxctl/run.go` (build store + pass to NewServer)
- Modify: `operator/cmd/sandboxctl/main.go` (dispatch `services apply`)

**Interfaces:**
- Consumes: `ParseServicesYAML` + `ValidateServiceSet` (Task 2); `client.Client` (run.go's `buildClient`); `v1alpha1.ServiceSet`/`ServiceSetSpec`; `EnvironmentRef` (`handlers.env`); `writeJSON`/`writeError`/`writeValidationError`/`decodeStrict` (handlers.go).
- Produces:
  - `type serviceSetApplier interface { Upsert(ctx context.Context, spec v1alpha1.ServiceSetSpec) error }` — `*serviceSetStore` is the real impl; handler tests use a fake.
  - `func newServiceSetStore(c client.Client, env EnvironmentRef) *serviceSetStore`.
  - `ServicesApplyRequest`/`ServicesApplyResponse` (api.go).
  - `ControlClient.ApplyServices(ctx, spec) (ServicesApplyResponse, error)` (control_client.go).
  - `RunServicesApply(args []string, getenv func(string) string, out io.Writer) int` (services_cli.go).
  - `NewServer` gains a `sets serviceSetApplier` parameter (insert after `env EnvironmentRef`); `run.go` builds the store and passes it; server/handler tests pass a fake.

- [ ] **Step 1: Add the request/response types**

In `operator/internal/sandboxctl/api.go`:
```go
// ServicesApplyRequest is the POST /v1/services body: the declaration (services
// + runtimes). EnvironmentName is NOT sent -- the server stamps it from its own
// identity, so a declaration is portable across environments.
type ServicesApplyRequest struct {
	Services []v1alpha1.ServiceSpec `json:"services,omitempty"`
	Runtimes []v1alpha1.RuntimeSpec  `json:"runtimes,omitempty"`
}

// ServicesApplyResponse is the POST /v1/services 200/201 body.
type ServicesApplyResponse struct {
	Environment string `json:"environment"`
	Services    int    `json:"services"`
	Runtimes    int    `json:"runtimes"`
	Applied     bool   `json:"applied"`
}
```
(Import `v1alpha1` if api.go doesn't already — it does.)

- [ ] **Step 2: Write the store envtest (failing)**

`operator/internal/sandboxctl/serviceset_store_test.go` (envtest — mirror the existing envtest setup in this package; the package already has envtest-based tests, e.g. `store_test.go` or a `_controller_test.go`; find and reuse the envtest bootstrap pattern with `KUBEBUILDER_ASSETS`). If no envtest helper exists in this package yet, add a small `testenv_test.go` that starts `envtest.TestServer` with the CRDs from `operator/config/crd/bases` and the v1alpha1 scheme, exposing a `testClient`/`testNS`. (Confirm by listing existing envtest usage: `grep -rln envtest internal/sandboxctl`.)

The test:
1. Starts envtest, creates a `SandboxEnvironment` named `env-1` in a namespace (so the OwnerRef UID resolves).
2. Builds `newServiceSetStore(c, EnvironmentRef{Name: "env-1", Namespace: ns})`.
3. Calls `Upsert(ctx, spec)` with `EnvironmentName` empty (the store sets it) and one service.
4. Asserts a `ServiceSet` named `env-1` exists with `Spec.EnvironmentName == "env-1"`, one service, and an OwnerReference to the `SandboxEnvironment` (Controller=true).
5. Calls `Upsert` again with a changed spec (two services) and asserts the same `ServiceSet` (by name) is updated (not duplicated), `Spec.Services` length 2, OwnerReference preserved.

- [ ] **Step 3: Run store test to verify it fails**

Run: `IN_CONTAINER=1 KUBEBUILDER_ASSETS=/tmp/envtest/k8s/1.35.0 go test ./internal/sandboxctl/ -run TestServiceSetStore -v`
Expected: FAIL — `newServiceSetStore`/`Upsert` undefined.

- [ ] **Step 4: Implement `serviceset_store.go`**

`operator/internal/sandboxctl/serviceset_store.go`:
```go
package sandboxctl

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// serviceSetApplier upserts the environment's ServiceSet CR. The only real
// implementation is *serviceSetStore; handler tests use a fake.
type serviceSetApplier interface {
	Upsert(ctx context.Context, spec v1alpha1.ServiceSetSpec) error
}

// serviceSetStore upserts the ServiceSet CR named after the environment,
// owned by the environment, with Spec.EnvironmentName == env.Name. The CR name
// == env.Name (one per environment) and the workspace PVC the runtimes mount is
// <env.Name>-workspace (owned by the env controller, referenced here by name).
type serviceSetStore struct {
	c   client.Client
	env types.NamespacedName // the SandboxEnvironment key (Name == ServiceSet name)
}

func newServiceSetStore(c client.Client, env EnvironmentRef) *serviceSetStore {
	return &serviceSetStore{c: c, env: types.NamespacedName{Name: env.Name, Namespace: env.Namespace}}
}

// Upsert sets spec.EnvironmentName from the store's env identity, Gets the
// SandboxEnvironment for its UID (the OwnerReference needs it; the env get is
// granted by the sidecar Role), then creates-or-updates the ServiceSet CR
// named env.Name. Update is by Get-then-Update (the sidecar Role grants
// get+create+update on servicesets resourceNames=[env.Name]).
func (s *serviceSetStore) Upsert(ctx context.Context, spec v1alpha1.ServiceSetSpec) error {
	spec.EnvironmentName = s.env.Name

	var env v1alpha1.SandboxEnvironment
	if err := s.c.Get(ctx, s.env, &env); err != nil {
		return fmt.Errorf("getting environment %s for ServiceSet ownership: %w", s.env.Name, err)
	}
	ownerRef := metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(), Kind: "SandboxEnvironment",
		Name: env.Name, UID: env.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}

	key := types.NamespacedName{Name: s.env.Name, Namespace: s.env.Namespace}
	var existing v1alpha1.ServiceSet
	err := s.c.Get(ctx, key, &existing)
	switch {
	case err == nil:
		existing.Spec = spec
		return s.c.Update(ctx, &existing)
	case apierrors.IsNotFound(err):
		ss := &v1alpha1.ServiceSet{
			ObjectMeta: metav1.ObjectMeta{Name: s.env.Name, Namespace: s.env.Namespace,
				OwnerReferences: []metav1.OwnerReference{ownerRef}},
			Spec: spec,
		}
		return s.c.Create(ctx, ss)
	default:
		return fmt.Errorf("getting ServiceSet %s: %w", s.env.Name, err)
	}
}
```

- [ ] **Step 5: Run store test to verify it passes**

Run: `IN_CONTAINER=1 KUBEBUILDER_ASSETS=/tmp/envtest/k8s/1.35.0 go test ./internal/sandboxctl/ -run TestServiceSetStore -v`
Expected: PASS.

- [ ] **Step 6: Add the rate-limit + body-size constants**

In `operator/internal/sandboxctl/limits.go`, append to the rate-limit `const` block:
```go
	servicesRatePerSec = 0.5 // apply is rare (on edits); allow a burst then throttle
	servicesBurst      = 3
```
and to the payload-size `const` block:
```go
	maxServicesBodyBytes = 256 << 10 // 256KiB: a declaration with many services/runtimes
```

- [ ] **Step 7: Wire the handler — `handleServicesApply`**

In `operator/internal/sandboxctl/handlers.go`, add a `sets serviceSetApplier` field to `handlers`:
```go
type handlers struct {
	store Store
	poll  *Poller
	env   EnvironmentRef
	sets  serviceSetApplier // nil for non-k8s envs (services apply then 404s/503s)
	now   func() time.Time
	log   func(format string, args ...any)
}
```
Add the handler:
```go
// handleServicesApply handles POST /v1/services. The agent POSTs a declaration
// (services + runtimes, no environmentName); the server validates it, stamps
// EnvironmentName from its own identity, and upserts the ServiceSet CR owned by
// the environment. The ServiceSetReconciler then reconciles it to Pods.
func (h *handlers) handleServicesApply(w http.ResponseWriter, r *http.Request) {
	if h.sets == nil {
		// Not a k8s-native environment: the sidecar Role grants no servicesets
		// RBAC, so upsert would 403. Surface it as a clean 404/503 rather than a
		// confusing 502 RBAC message.
		writeError(w, http.StatusNotFound, CodeNotFound, "services are not enabled on this environment (requires the k8s-native engine)", "", nil)
		return
	}
	var req ServicesApplyRequest
	if err := decodeStrict(r.Body, &req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	spec := v1alpha1.ServiceSetSpec{EnvironmentName: h.env.Name, Services: req.Services, Runtimes: req.Runtimes}
	if err := ValidateServiceSet(spec); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			writeValidationError(w, http.StatusUnprocessableEntity, ve)
			return
		}
		writeError(w, http.StatusBadRequest, CodeInvalidDeclaration, err.Error(), "", nil)
		return
	}
	if err := h.sets.Upsert(r.Context(), spec); err != nil {
		h.log("services apply failed: %v", err)
		status := http.StatusBadGateway
		code := CodeServiceSetUpsertFailed
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsNotFound(err) {
			status = http.StatusBadGateway
		}
		writeError(w, status, code, err.Error(), "", nil)
		return
	}
	writeJSON(w, http.StatusOK, ServicesApplyResponse{
		Environment: h.env.Name, Services: len(spec.Services), Runtimes: len(spec.Runtimes), Applied: true,
	})
}
```
(Ensure `errors`, `apierrors`, `v1alpha1` are imported — `v1alpha1` already is; add `apierrors "k8s.io/apimachinery/pkg/api/errors"` if missing.)

- [ ] **Step 8: Register the route in `server.go`**

In `NewServer`, add a `servicesBucket := newTokenBucket(servicesRatePerSec, servicesBurst)` near the other buckets, and after the `/v1/progress` registration add:
```go
	mux.Handle("POST /v1/services", postChain(http.HandlerFunc(h.handleServicesApply), log, servicesBucket, maxServicesBodyBytes))
```
Add a bare `methodNotAllowed` registration for `/v1/services` alongside the others:
```go
	mux.HandleFunc("/v1/services", methodNotAllowed)
```
Change the `NewServer` signature to accept the applier (insert after `env EnvironmentRef`):
```go
func NewServer(cfg Config, store Store, poll *Poller, env EnvironmentRef, sets serviceSetApplier, now func() time.Time, log func(format string, args ...any)) *http.Server {
```
and set `h := &handlers{store: store, poll: poll, env: env, sets: sets, now: now, log: log}`.

- [ ] **Step 9: Wire `run.go`**

In `Run`, after building `c` and `env`, add:
```go
	sets := newServiceSetStore(c, env)
```
and update the `NewServer` call:
```go
	srv := NewServer(cfg, store, poll, env, sets, time.Now, logf)
```
Find and update any OTHER `NewServer` call site (grep `NewServer(`) — there is at least `server_test.go`; those pass a fake/nil `sets` (see Step 10).

- [ ] **Step 10: Update existing server/handler tests**

`grep -rn "NewServer(" operator/` and update every call to pass a `sets` argument. For tests that don't exercise `/v1/services`, pass `nil` (the handler's `h.sets == nil` branch returns a clean 404). Add a handler test `TestHandleServicesApply` in `handlers_test.go` (or `services_cli_test.go`) using `httptest` + a fake `serviceSetApplier`:
```go
type fakeApplier struct{ got v1alpha1.ServiceSetSpec; err error}
func (f *fakeApplier) Upsert(_ context.Context, s v1alpha1.ServiceSetSpec) error { f.got = s; return f.err }
```
Assert: a valid declaration → 200 + `Applied:true` + correct counts; a cross-list collision → 422 + `CodeDuplicateEntryName`; `fakeApplier.err` set to a forbidden-like error → 502 + `CodeServiceSetUpsertFailed`.

- [ ] **Step 11: Implement the loopback `ControlClient`**

`operator/internal/sandboxctl/control_client.go`:
```go
package sandboxctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ControlClient is the agent's loopback HTTP client to the control API. It
// holds no Kubernetes credential -- only an HTTP connection to 127.0.0.1:9099.
type ControlClient struct {
	addr string
	http *http.Client
}

func NewControlClient(addr string) *ControlClient {
	return &ControlClient{addr: addr, http: &http.Client{Timeout: 0}} // timeout via ctx
}

// ApplyServices POSTs a declaration to /v1/services and returns the response.
func (c *ControlClient) ApplyServices(ctx context.Context, req ServicesApplyRequest) (ServicesApplyResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return ServicesApplyResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.addr+"/v1/services", bytes.NewReader(body))
	if err != nil {
		return ServicesApplyResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	return doJSON[ServicesApplyResponse](ctx, c.http, httpReq)
}

// doJSON executes req and decodes either the success body into T or the
// ErrorEnvelope on a non-2xx.
func doJSON[T any](_ context.Context, h *http.Client, req *http.Request) (T, error) {
	var zero T
	resp, err := h.Do(req)
	if err != nil {
		return zero, fmt.Errorf("control API %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out T
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return zero, fmt.Errorf("decoding control API response: %w", err)
		}
		return out, nil
	}
	var env ErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	msg := env.Error.Message
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return zero, &ControlAPIError{Code: env.Error.Code, Status: resp.StatusCode, Message: msg}
}

// ControlAPIError is returned by ControlClient on a non-2xx response, carrying
// the server's error code so the CLI can print an actionable message.
type ControlAPIError struct {
	Code    string
	Status  int
	Message string
}

func (e *ControlAPIError) Error() string { return e.Message }
```
(Go 1.18+ generics are fine; confirm the module's Go version is >=1.22 per Global Constraints.)

- [ ] **Step 12: Implement the CLI entrypoint**

`operator/internal/sandboxctl/services_cli.go`:
```go
package sandboxctl

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/go-logr/logr"
)

// RunServicesApply implements `sandboxctl services apply [flags] [file]`.
// Default file is ./services.yaml. It parses + validates client-side, POSTs to
// the loopback control API, and prints the result. A validation error is
// printed to stderr and exits 2; a control-API error exits 1.
func RunServicesApply(args []string, getenv func(string) string, out io.Writer) int {
	fs := newFlagSet("sandboxctl services apply")
	listen := fs.String("listen", envOr(getenv, "LISTEN", "127.0.0.1:9099"), "control API address")
	file := fs.String("file", "", "path to services.yaml (default ./services.yaml)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "invalid flags: "+err.Error())
		return 2
	}
	if err := validateLoopbackListen(*listen); err != nil {
		fmt.Fprintln(os.Stderr, "invalid --listen: "+err.Error())
		return 2
	}
	path := *file
	if path == "" {
		if fs.NArg() > 0 {
			path = fs.Arg(0)
		} else {
			path = "services.yaml"
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading "+path+": "+err.Error())
		return 2
	}
	spec, err := ParseServicesYAML(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parsing "+path+": "+err.Error())
		return 2
	}
	spec.EnvironmentName = "local" // client-side pre-check placeholder; server overrides
	if err := ValidateServiceSet(spec); err != nil {
		fmt.Fprintln(os.Stderr, "invalid declaration: "+err.Error())
		return 2
	}
	cli := NewControlClient(*listen)
	resp, err := cli.ApplyServices(context.Background(), ServicesApplyRequest{Services: spec.Services, Runtimes: spec.Runtimes})
	if err != nil {
		fmt.Fprintln(os.Stderr, "services apply failed: "+err.Error())
		return 1
	}
	fmt.Fprintf(out, "applied environment=%s services=%d runtimes=%d\n", resp.Environment, resp.Services, resp.Runtimes)
	return 0
}

// RunServicesCompose implements `sandboxctl services compose [flags] [file]`.
// Pure: renders docker-compose.yml to stdout (or -o).
func RunServicesCompose(args []string, getenv func(string) string, out io.Writer) int {
	fs := newFlagSet("sandboxctl services compose")
	file := fs.String("file", "", "path to services.yaml (default ./services.yaml)")
	output := fs.String("o", "", "write to file instead of stdout")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "invalid flags: "+err.Error())
		return 2
	}
	path := *file
	if path == "" {
		if fs.NArg() > 0 {
			path = fs.Arg(0)
		} else {
			path = "services.yaml"
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading "+path+": "+err.Error())
		return 2
	}
	spec, err := ParseServicesYAML(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parsing "+path+": "+err.Error())
		return 2
	}
	rendered, err := Compose(spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rendering compose: "+err.Error())
		return 1
	}
	if *output == "" {
		_, _ = out.Write(rendered)
		return 0
	}
	if err := os.WriteFile(*output, rendered, 0o644); err != nil { //nolint:gosec // G306: compose YAML, non-secret
		fmt.Fprintln(os.Stderr, "writing "+*output+": "+err.Error())
		return 1
	}
	return 0
}

// newFlagSet mirrors config.go's flag.NewFlagSet usage with a consistent name
// and ContinueOnError.
func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}
```
(Add `import "flag"`. The `logr` import above is unused here — drop it.)

- [ ] **Step 13: Write the CLI test (httptest)**

`operator/internal/sandboxctl/services_cli_test.go`: spin up an `httptest.NewServer` whose handler asserts the POST body decodes to a `ServicesApplyRequest` with one service, then returns 200 + a `ServicesApplyResponse`. Point the CLI at it by writing a temp `services.yaml` and calling `RunServicesApply` with `--listen` set to the test server's `Listener.Addr().String()`. Assert exit 0 and stdout contains `applied`. Also test a 422 path (server returns a `CodeDuplicateEntryName` envelope) → exit 2, stderr contains the message. (Note: `RunServicesApply` uses `http://<listen>/v1/services`; the httptest server URL is `http://127.0.0.1:<port>`, so pass `--listen 127.0.0.1:<port>`.)

- [ ] **Step 14: Dispatch `services apply`/`compose` from `main.go`**

In `operator/cmd/sandboxctl/main.go`, add a nested `case "services":` to the `switch args[1]`:
```go
	case "services":
		if len(args) < 3 {
			_, _ = fmt.Fprintln(os.Stderr, "usage: sandboxctl services <apply|compose> [flags] [file]")
			return 2
		}
		switch args[2] {
		case "apply":
			return sandboxctl.RunServicesApply(args[3:], os.Getenv, os.Stdout)
		case "compose":
			return sandboxctl.RunServicesCompose(args[3:], os.Getenv, os.Stdout)
		default:
			_, _ = fmt.Fprintf(os.Stderr, "unknown services subcommand %q\n", args[2])
			return 2
		}
```
Update the usage strings in `run` (the `len(args) < 2` branch and the default branch) to include `services` and `exec`.

- [ ] **Step 15: Run the full sandboxctl + render suites**

Run: `IN_CONTAINER=1 KUBEBUILDER_ASSETS=/tmp/envtest/k8s/1.35.0 go test ./internal/sandboxctl/ ./internal/render/ ./cmd/sandboxctl/ -count=1`
Expected: PASS (envtest tests need the assets env var).

- [ ] **Step 16: Commit**

```bash
git add internal/sandboxctl/serviceset_store.go internal/sandboxctl/serviceset_store_test.go internal/sandboxctl/control_client.go internal/sandboxctl/control_client_test.go internal/sandboxctl/services_cli.go internal/sandboxctl/services_cli_test.go internal/sandboxctl/api.go internal/sandboxctl/limits.go internal/sandboxctl/server.go internal/sandboxctl/handlers.go internal/sandboxctl/run.go cmd/sandboxctl/main.go
git commit -m "feat(sandboxctl): services apply (CLI + POST /v1/services + ServiceSet upsert)

Refs #24 (Plan 2 of 3; does not close)"
```

---

## Task 6: `sandboxctl exec` — CLI + `POST /v1/exec` + SPDY `Execer` (one-shot)

One-shot exec: the CLI POSTs `{runtime, command, stdin}` to `POST /v1/exec`; the server's `Execer` runs the command in the runtime pod via SPDY (`remotecommand`, mirroring `test/e2e/harness.go`'s `Exec`) and returns stdout/stderr + a best-effort exit code. Interactive/streaming/TTY is deferred (YAGNI — the agent's need is run-command-get-output). `stdin` is text in the request body (binary stdin deferred).

**Files:**
- Create: `operator/internal/sandboxctl/exec.go`
- Create: `operator/internal/sandboxctl/exec_test.go` (httptest + fake Execer)
- Modify: `operator/internal/sandboxctl/api.go` (`ExecRequest`/`ExecResponse`)
- Modify: `operator/internal/sandboxctl/limits.go` (exec bucket + body size)
- Modify: `operator/internal/sandboxctl/server.go` (route + `Execer` param)
- Modify: `operator/internal/sandboxctl/handlers.go` (`handlers.execer` + `handleExec`)
- Modify: `operator/internal/sandboxctl/run.go` (build `podExecer` + pass to NewServer)
- Modify: `operator/internal/sandboxctl/control_client.go` (`Exec` method)
- Modify: `operator/internal/sandboxctl/services_cli.go` (`RunExec`)
- Modify: `operator/cmd/sandboxctl/main.go` (dispatch `exec`)

**Interfaces:**
- Consumes: `remotecommand` (`k8s.io/client-go/tools/remotecommand`), `kubernetes.Clientset` (`k8s.io/client-go/kubernetes`), `corev1.PodExecOptions`, `clientgoscheme.ParameterCodec` (mirror `test/e2e/harness.go:588-614`); `rest.Config` (run.go's `rest.InClusterConfig`); `EnvironmentRef` (`handlers.env` → namespace).
- Produces:
  - `type Execer interface { Exec(ctx context.Context, podName string, cmd []string, stdin []byte) (stdout, stderr []byte, err error) }` — container is implicit (== podName, since the controller names the runtime's single container after the entry).
  - `func newPodExecer(restCfg *rest.Config, namespace string) *podExecer`.
  - `ExecRequest`/`ExecResponse` (api.go).
  - `ControlClient.Exec(ctx, req ExecRequest) (ExecResponse, error)`.
  - `RunExec(args, getenv, stdin, stdout, stderr) int`.
  - `NewServer` gains an `execer Execer` parameter (after `sets`); `run.go` builds it; handler/server tests pass a fake/nil.

- [ ] **Step 1: Add request/response types**

In `operator/internal/sandboxctl/api.go`:
```go
// ExecRequest is the POST /v1/exec body: run cmd in the named runtime pod,
// piping stdin (text) into the process. One-shot (no TTY/streaming).
type ExecRequest struct {
	Runtime string   `json:"runtime"`
	Command []string `json:"command"`
	Stdin   string   `json:"stdin,omitempty"`
}

// ExecResponse is the POST /v1/exec 200 body. stdout/stderr are the command's
// captured output (text; binary output is out of scope). ExitCode is
// best-effort: 0 on success, the command's exit code when extractable from the
// SPDY error, else -1. Error is the transport/protocol error message, empty
// when the command ran (regardless of its exit code).
type ExecResponse struct {
	Runtime  string `json:"runtime"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}
```

- [ ] **Step 2: Write the handler test (failing)**

`operator/internal/sandboxctl/exec_test.go` (httptest with a fake `Execer`; NO envtest needed for the handler path):
```go
package sandboxctl

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeExecer struct {
	stdout, stderr []byte
	err            error
	gotPod         string
	gotCmd         []string
	gotStdin       []byte
}

func (f *fakeExecer) Exec(_ context.Context, pod string, cmd []string, stdin []byte) ([]byte, []byte, error) {
	f.gotPod, f.gotCmd, f.gotStdin = pod, cmd, stdin
	return f.stdout, f.stderr, f.err
}

func TestHandleExec(t *testing.T) {
	h := &handlers{env: EnvironmentRef{Name: "env-1", Namespace: "ns"}, execer: &fakeExecer{stdout: []byte("hello\n"), stderr: nil}}
	srv := httptest.NewServer(buildTestMux(h)) // helper wiring /v1/exec -> handleExec with postChain
	defer srv.Close()

	body := `{"runtime":"python","command":["echo","hi"],"stdin":"pipe"}`
	resp, err := http.Post(srv.URL+"/v1/exec", "application/json", strings.NewReader(body))
	if err != nil { t.Fatal(err) }
	if resp.StatusCode != http.StatusOK { t.Fatalf("status %d", resp.StatusCode) }
	// decode ExecResponse, assert stdout == "hello\n", runtime echoed
	_ = resp.Body.Close()
}
```
(`buildTestMux` is a tiny test helper you add in `exec_test.go` that wires a single route through `postChain` to the handler — or reuse the existing `NewServer` with a fake execer and post to it. Prefer `NewServer` for fidelity: build `NewServer(Config{Listen:"127.0.0.1:0"}, nil, nil, EnvironmentRef{...}, nil, &fakeExecer{}, time.Now, nil)`-style and use `httptest.NewServer(srv.Handler)`.) Add assertions: empty `runtime` → 400 `CodeMissingParam`; empty `command` → 400; `execer.err` set → 200 with `Error` populated.

- [ ] **Step 3: Run test to verify it fails**

Run: `IN_CONTAINER=1 go test ./internal/sandboxctl/ -run TestHandleExec -v`
Expected: FAIL — `Execer`/`handleExec`/`ExecRequest` undefined.

- [ ] **Step 4: Implement `exec.go`**

`operator/internal/sandboxctl/exec.go`:
```go
package sandboxctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// Execer runs a one-shot command inside a runtime pod. The container is
// implicit: the ServiceSetReconciler names a runtime pod's single container
// after the entry name (== podName), so podName doubles as the container name.
// Interactive/streaming/TTY exec is deferred; this is run-command-get-output.
type Execer interface {
	Exec(ctx context.Context, podName string, cmd []string, stdin []byte) (stdout, stderr []byte, err error)
}

// podExecer is the real Execer: a client-go SPDY exec, mirroring
// test/e2e/harness.go's Exec exactly, with stdin piped from a bytes.Reader.
type podExecer struct {
	restCfg   *rest.Config
	namespace string
}

func newPodExecer(restCfg *rest.Config, namespace string) *podExecer {
	return &podExecer{restCfg: restCfg, namespace: namespace}
}

func (e *podExecer) Exec(ctx context.Context, podName string, cmd []string, stdin []byte) ([]byte, []byte, error) {
	cs, err := kubernetes.NewForConfig(e.restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("building clientset: %w", err)
	}
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(e.namespace).
		Name(podName).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: podName,
		Command:   cmd,
		Stdin:     len(stdin) > 0,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(e.restCfg, "POST", req.URL())
	if err != nil {
		return nil, nil, fmt.Errorf("building SPDY executor: %w", err)
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	opts := remotecommand.StreamOptions{Stdout: &stdoutBuf, Stderr: &stderrBuf}
	if len(stdin) > 0 {
		opts.Stdin = bytes.NewReader(stdin)
	}
	err = executor.StreamWithContext(ctx, opts)
	return stdoutBuf.Bytes(), stderrBuf.Bytes(), err
}

// extractExitCode best-effort-extracts a command exit code from a remotecommand
// error. remotecommand surfaces a non-zero exit as a util/exec.CodeExitError;
// anything else is a transport/protocol failure (returns -1).
func extractExitCode(err error) int {
	if err == nil {
		return 0
	}
	var cee utilexec.CodeExitError
	if errors.As(err, &cee) {
		return cee.Code
	}
	return -1
}
```
(Confirm `utilexec.CodeExitError` has a `Code int` field — read `k8s.io/client-go/util/exec` if the compiler complains; if it instead exposes `ExitStatus() int`, use that. The handler uses `extractExitCode` so this is the only place the contract is probed.)

- [ ] **Step 5: Add the exec rate-limit + body-size constants**

In `limits.go`, append:
```go
	execRatePerSec = 2.0
	execBurst      = 5
```
and
```go
	maxExecBodyBytes = 1 << 20 // 1MiB: command + (text) stdin
```

- [ ] **Step 6: Add `handleExec` to handlers.go**

Add an `execer Execer` field to `handlers`:
```go
type handlers struct {
	store  Store
	poll   *Poller
	env    EnvironmentRef
	sets   serviceSetApplier
	execer Execer // nil for non-k8s envs
	now    func() time.Time
	log    func(format string, args ...any)
}
```
Add:
```go
// handleExec handles POST /v1/exec. The agent POSTs {runtime, command, stdin};
// the server one-shot execs into the named runtime pod (SPDY) and returns
// stdout/stderr + a best-effort exit code. A non-zero command exit is NOT a
// server error: stdout/stderr are returned with ExitCode set and Error empty.
// A transport/protocol failure (pod gone, not authorized) returns 200 with
// Error populated so the agent sees the failure message alongside any output.
func (h *handlers) handleExec(w http.ResponseWriter, r *http.Request) {
	if h.execer == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "exec is not enabled on this environment (requires the k8s-native engine)", "", nil)
		return
	}
	var req ExecRequest
	if err := decodeStrict(r.Body, &req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	if req.Runtime == "" {
		writeError(w, http.StatusBadRequest, CodeMissingParam, "runtime must not be empty", "runtime", nil)
		return
	}
	if len(req.Command) == 0 {
		writeError(w, http.StatusBadRequest, CodeMissingParam, "command must not be empty", "command", nil)
		return
	}
	stdout, stderr, err := h.execer.Exec(r.Context(), req.Runtime, req.Command, []byte(req.Stdin))
	resp := ExecResponse{Runtime: req.Runtime, Stdout: string(stdout), Stderr: string(stderr), ExitCode: extractExitCode(err)}
	if err != nil && resp.ExitCode < 0 {
		h.log("exec %s failed: %v", req.Runtime, err)
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}
```

- [ ] **Step 7: Register the route + widen `NewServer`**

In `server.go`, add `execBucket := newTokenBucket(execRatePerSec, execBurst)`, register:
```go
	mux.Handle("POST /v1/exec", postChain(http.HandlerFunc(h.handleExec), log, execBucket, maxExecBodyBytes))
```
add the bare `mux.HandleFunc("/v1/exec", methodNotAllowed)`, and widen `NewServer` to take `execer Execer` after `sets`:
```go
func NewServer(cfg Config, store Store, poll *Poller, env EnvironmentRef, sets serviceSetApplier, execer Execer, now func() time.Time, log func(format string, args ...any)) *http.Server {
```
with `h := &handlers{store: store, poll: poll, env: env, sets: sets, execer: execer, now: now, log: log}`.

- [ ] **Step 8: Wire `run.go`**

`buildClient` currently builds only a `client.Client` and discards the `rest.Config`. Refactor minimally so `Run` has the `rest.Config` for the execer. Add a sibling helper and call it in `Run`:
```go
func buildRestConfig() (*rest.Config, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("loading in-cluster config: %w", err)
	}
	return restCfg, nil
}
```
In `Run`, build the restCfg once and derive both the client and the execer from it (or keep `buildClient` for the client and call `buildRestConfig` for the execer — a second `InClusterConfig()` call is cheap and keeps the diff local). Then:
```go
	restCfg, err := rest.InClusterConfig()
	if err != nil { return fmt.Errorf("loading in-cluster config: %w", err) }
	// ... build c from restCfg (or keep buildClient) ...
	execer := newPodExecer(restCfg, cfg.Namespace)
	srv := NewServer(cfg, store, poll, env, sets, execer, time.Now, logf)
```
(Choose the option that minimizes the diff: calling `rest.InClusterConfig()` once more in `Run` for the execer, while leaving `buildClient` untouched, is the smallest change. Do that.)

- [ ] **Step 9: Add `ControlClient.Exec` + `RunExec`**

In `control_client.go`:
```go
// Exec POSTs an ExecRequest to /v1/exec and returns the response.
func (c *ControlClient) Exec(ctx context.Context, req ExecRequest) (ExecResponse, error) {
	return postJSON[ExecResponse](ctx, c.http, c.addr, "/v1/exec", req)
}
```
(Refactor `ApplyServices` to share a `postJSON[T]` helper if it tidies the duplication; otherwise inline as in Step 11 of Task 5.)

In `services_cli.go`:
```go
// RunExec implements `sandboxctl exec [flags] <runtime> -- <cmd...>`.
// Stdin is read from the passed stdin reader and sent as text. The command's
// stdout/stderr are written to the respective writers; the process exit code is
// mapped from the response's best-effort ExitCode (-1 -> 1).
func RunExec(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("sandboxctl exec")
	listen := fs.String("listen", envOr(getenv, "LISTEN", "127.0.0.1:9099"), "control API address")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "invalid flags: "+err.Error())
		return 2
	}
	if err := validateLoopbackListen(*listen); err != nil {
		fmt.Fprintln(os.Stderr, "invalid --listen: "+err.Error())
		return 2
	}
	rest := fs.Args()
	// Expect: <runtime> -- <cmd...>
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: sandboxctl exec <runtime> -- <cmd...>")
		return 2
	}
	runtime := rest[0]
	cmd := rest[1:]
	// Drop a leading "--" separator if present.
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 {
		fmt.Fprintln(os.Stderr, "usage: sandboxctl exec <runtime> -- <cmd...>")
		return 2
	}
	var stdinBytes []byte
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		// stdin is a pipe/file: read it.
		stdinBytes, _ = io.ReadAll(stdin)
	}
	cli := NewControlClient(*listen)
	resp, err := cli.Exec(context.Background(), ExecRequest{Runtime: runtime, Command: cmd, Stdin: string(stdinBytes)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "exec failed: "+err.Error())
		return 1
	}
	_, _ = stdout.Write([]byte(resp.Stdout))
	_, _ = stderr.Write([]byte(resp.Stderr))
	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, "exec error: "+resp.Error)
		return 1
	}
	if resp.ExitCode < 0 {
		return 1
	}
	return resp.ExitCode
}
```
(Note: reading stdin via `os.Stdin` directly here couples to the process stdin; for testability, `RunExec` takes an `stdin io.Reader` and the stdin-pipe check reads from it. Adjust the `os.Stdin.Stat()` check to stat the passed reader if it's an `*os.File`, else always read. Keep it simple: read the passed `stdin` fully when non-empty; the test passes `strings.NewReader`.)

- [ ] **Step 10: Write the CLI exec test**

In `services_cli_test.go` add `TestRunExec`: httptest server returning 200 `ExecResponse{Stdout:"out\n", ExitCode:0}` → assert `RunExec` exit 0 and stdout == `out\n`; a response with `ExitCode:3` → exit 3; a response with `Error:"boom"` → exit 1 + stderr contains `boom`.

- [ ] **Step 11: Dispatch `exec` from `main.go`**

In the `switch args[1]`, add:
```go
	case "exec":
		return sandboxctl.RunExec(args[2:], os.Getenv, os.Stdin, os.Stdout, os.Stderr)
```
Update the usage strings to include `exec`.

- [ ] **Step 12: Run the suites**

Run: `IN_CONTAINER=1 KUBEBUILDER_ASSETS=/tmp/envtest/k8s/1.35.0 go test ./internal/sandboxctl/ ./internal/render/ ./cmd/sandboxctl/ -count=1`
Expected: PASS.

- [ ] **Step 13: Commit**

```bash
git add internal/sandboxctl/exec.go internal/sandboxctl/exec_test.go internal/sandboxctl/api.go internal/sandboxctl/limits.go internal/sandboxctl/server.go internal/sandboxctl/handlers.go internal/sandboxctl/run.go internal/sandboxctl/control_client.go internal/sandboxctl/services_cli.go internal/sandboxctl/services_cli_test.go cmd/sandboxctl/main.go
git commit -m "feat(sandboxctl): exec (CLI + POST /v1/exec + SPDY one-shot Execer)

Refs #24 (Plan 2 of 3; does not close)"
```

---

## Task 7: `ServiceSetReconciler` defense-in-depth guard for cross-list name collision (#2)

The #2 defect: a service and a runtime sharing a name both target `Pod/<name>` — the two `ensurePod` calls delete+recreate each other's pod in an infinite loop (a reconcile storm). Admission (Task 2/5) rejects this, but a hand-crafted CR that bypasses admission must still be safe. The guard detects a cross-list (or within-list) duplicate before reconciling children, writes `Ready=False` with reason `DuplicateEntryName`, and returns nil (no child reconcile, no storm, no requeue). The user fixes the spec; the next reconcile proceeds.

**Files:**
- Modify: `operator/internal/controller/serviceset_controller.go` (`Reconcile` guard + `duplicateEntryName` + `writeDuplicateStatus`)
- Test: `operator/internal/controller/serviceset_controller_test.go` (envtest — add to the existing Plan 1 test file)

**Interfaces:**
- Consumes: `ServiceSet.Spec.Services`/`Runtimes`; the existing `writeStatus`/`apimeta.SetStatusCondition` pattern.
- Produces: `func duplicateEntryName(ss *v1alpha1.ServiceSet) string` (first colliding name, "" if none); `Reconcile` short-circuits on collision.

- [ ] **Step 1: Read the existing controller test file**

`grep -n "func Test" operator/internal/controller/serviceset_controller_test.go` — mirror its envtest setup (it creates a `ServiceSet`, runs the reconciler, asserts children). The guard test creates a `ServiceSet` with `Services:[{Name:"x"}]` + `Runtimes:[{Name:"x"}]`, reconciles, and asserts (a) no `Pod/x` exists, (b) the `Ready` condition is `False` with reason `DuplicateEntryName`, (c) a second `Reconcile` returns nil and still no `Pod/x` (no storm).

- [ ] **Step 2: Write the failing test**

Add to `serviceset_controller_test.go`:
```go
func TestServiceSetReconcileDuplicateEntryName(t *testing.T) {
	// envtest bootstrap as the existing tests use; create the ServiceSet below
	// and call r.Reconcile(ctx, req) twice.
	ss := &sandboxv1alpha1.ServiceSet{
		ObjectMeta: metav1.ObjectMeta{Name: "env-1", Namespace: ns},
		Spec: sandboxv1alpha1.ServiceSetSpec{EnvironmentName: "env-1",
			Services: []sandboxv1alpha1.ServiceSpec{{Name: "shared", Image: "a"}},
			Runtimes: []sandboxv1alpha1.RuntimeSpec{{Name: "shared", Image: "b"}}},
	}
	// create ss, reconcile, assert Ready=False reason DuplicateEntryName, no Pod/shared.
	// reconcile again, assert nil error and still no Pod/shared (no storm).
}
```
(Use the file's existing helpers for envtest client + namespace; if it creates the workspace PVC stub, mirror that.)

- [ ] **Step 3: Run test to verify it fails**

Run: `IN_CONTAINER=1 KUBEBUILDER_ASSETS=/tmp/envtest/k8s/1.35.0 go test ./internal/controller/ -run TestServiceSetReconcileDuplicateEntryName -v`
Expected: FAIL — `Ready` is `EntriesNotReady`/a pod is created / storm (the guard does not exist yet).

- [ ] **Step 4: Implement the guard**

In `serviceset_controller.go`, add at the top of `Reconcile` (after the `Get` and before the service/runtime reconcile loops):
```go
	if dup := duplicateEntryName(&ss); dup != "" {
		if err := r.writeDuplicateStatus(ctx, &ss, dup); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil // no child reconcile: a colliding name would
		// storm (two ensurePod calls targeting the same Pod/<name>), so we
		// refuse to create children and wait for the spec to be fixed. Stale
		// children from before the collision are owned by the env and GC'd on
		// env deletion; the next reconcile after the fix prunes + reconciles.
	}
```
Add the helpers:
```go
// duplicateEntryName returns the first entry name that appears more than once
// across services+runtimes (the #2 defect: the CRD pins uniqueness within each
// list only, so a service+runtime sharing a name passes the API server but would
// storm the reconciler). "" means no collision.
func duplicateEntryName(ss *sandboxv1alpha1.ServiceSet) string {
	count := map[string]int{}
	for _, s := range ss.Spec.Services {
		count[s.Name]++
	}
	for _, rt := range ss.Spec.Runtimes {
		count[rt.Name]++
	}
	for name, n := range count {
		if n > 1 {
			return name
		}
	}
	return ""
}

// writeDuplicateStatus marks every entry NotReady and the ServiceSet Ready=False
// with reason DuplicateEntryName, without reconciling any children. It is the
// defense-in-depth counterpart to admission's ValidateServiceSet: a hand-crafted
// CR that bypasses admission still fails safe rather than storming.
func (r *ServiceSetReconciler) writeDuplicateStatus(ctx context.Context, ss *sandboxv1alpha1.ServiceSet, dup string) error {
	base := ss.DeepCopy()
	entries := make([]sandboxv1alpha1.EntryStatus, 0, len(ss.Spec.Services)+len(ss.Spec.Runtimes))
	for _, s := range ss.Spec.Services {
		entries = append(entries, sandboxv1alpha1.EntryStatus{Name: s.Name, Kind: "service", Ready: false, Reason: "DuplicateEntryName"})
	}
	for _, rt := range ss.Spec.Runtimes {
		entries = append(entries, sandboxv1alpha1.EntryStatus{Name: rt.Name, Kind: "runtime", Ready: false, Reason: "DuplicateEntryName"})
	}
	apimeta.SetStatusCondition(&ss.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: "DuplicateEntryName",
		Message: fmt.Sprintf("entry name %q is duplicated across services+runtimes", dup),
		LastTransitionTime: metav1.Now(), ObservedGeneration: ss.Generation,
	})
	ss.Status.Entries = entries
	return r.Status().Patch(ctx, ss, client.MergeFrom(base))
}
```

- [ ] **Step 5: Run test to verify it passes + run the full controller suite**

Run: `IN_CONTAINER=1 KUBEBUILDER_ASSETS=/tmp/envtest/k8s/1.35.0 go test ./internal/controller/ -count=1`
Expected: PASS (the new test + all existing Plan 1 controller tests).

- [ ] **Step 6: Commit**

```bash
git add internal/controller/serviceset_controller.go internal/controller/serviceset_controller_test.go
git commit -m "fix(controller): guard ServiceSet cross-list name collision (#2, defense-in-depth)

Refs #24 (Plan 2 of 3; does not close)"
```

---

## Task 8: NetworkPolicy integration — dep/runtime pods inherit env isolation

The design asserts dep/runtime pods "inherit the namespace's NetworkPolicy." Today the env NetworkPolicy selects ONLY the agent pod (by the env label), so dep/runtime pods are NOT selected → unrestricted. Fix: (1) label every ServiceSet child pod with the env label (`sandbox.psenna.dev/environment: render.EnvironmentLabelValue(ss.Spec.EnvironmentName)`) so the Restricted default-deny applies to them; (2) set `automountServiceAccountToken:false` on those pods (they hold no credential — same invariant as the agent); (3) add an env-egress rule to the Restricted NetworkPolicy letting the agent reach env-labeled pods (so the agent connects to deps via Service DNS, and the dep/runtime pods' egress default-deny allows them to reach each other + kube-dns).

**Files:**
- Modify: `operator/internal/controller/serviceset_controller.go` (`entryLabels` adds env label; `ensurePod` sets `automountServiceAccountToken:false`)
- Modify: `operator/internal/render/networkpolicy.go` (add env-egress rule)
- Test: `operator/internal/controller/serviceset_controller_test.go` (assert env label + automount false on a created pod)
- Test: `operator/internal/render/networkpolicy_test.go` (assert the new egress rule)

**Interfaces:**
- Consumes: `render.EnvironmentLabelValue(envName)` (from `render/names.go`); `ptr.To(false)` for `AutomountServiceAccountToken`; the existing `RenderNetworkPolicy` egress slice.
- Produces: every ServiceSet child pod carries the env label + `AutomountServiceAccountToken=false`; the Restricted NetworkPolicy has an egress rule `to: {podSelector: matchLabels{sandbox.psenna.dev/environment: <envLabelValue>}}`.

- [ ] **Step 1: Write the failing controller test**

Add to `serviceset_controller_test.go` a test that reconciles a `ServiceSet` with one service + one runtime and asserts the created Pods carry the label `sandbox.psenna.dev/environment` == `render.EnvironmentLabelValue("env-1")` AND `pod.Spec.AutomountServiceAccountToken` is a pointer to `false`. (Use `render.EnvironmentLabelValue` in the assertion so it stays in sync with the helper.)

- [ ] **Step 2: Run to verify it fails**

Run: `IN_CONTAINER=1 KUBEBUILDER_ASSETS=/tmp/envtest/k8s/1.35.0 go test ./internal/controller/ -run TestServiceSetPodEnvLabelAndNoToken -v`
Expected: FAIL — no env label / no automount false.

- [ ] **Step 3: Add the env label + automount false in the controller**

In `serviceset_controller.go`, add the env label in `entryLabels`:
```go
func entryLabels(ss *sandboxv1alpha1.ServiceSet, name, kind string) map[string]string {
	return map[string]string{
		labelServiceset: ss.Name,
		labelEntry:      name,
		labelKind:       kind,
		"sandbox.psenna.dev/environment": render.EnvironmentLabelValue(ss.Spec.EnvironmentName),
	}
}
```
Add the `render` import (`"github.com/psenna/ai-sandbox/operator/internal/render"`) to the controller package if not present (it likely is not — controller currently imports only `v1alpha1` + k8s libs; add it).

In `ensurePod`, set automount false on the PodSpec (add after the pod struct literal, before `applySecurityContext`):
```go
	pod.Spec.AutomountServiceAccountToken = ptr.To(false)
```
(`ptr` is already imported in the file — `k8s.io/utils/ptr`.)

- [ ] **Step 4: Run the controller test to verify it passes**

Run: `IN_CONTAINER=1 KUBEBUILDER_ASSETS=/tmp/envtest/k8s/1.35.0 go test ./internal/controller/ -count=1`
Expected: PASS (new test + existing).

- [ ] **Step 5: Write the failing networkpolicy test**

Add to `networkpolicy_test.go` a test that builds Restricted `Inputs` and asserts one egress rule has `To` with a `PodSelector` matching `{sandbox.psenna.dev/environment: <EnvironmentLabelValue(env.Name)>}` and no ports (all ports). (Mirror the existing NP test's assertion style.)

- [ ] **Step 6: Run to verify it fails**

Run: `IN_CONTAINER=1 go test ./internal/render/ -run TestNetworkPolicyEnvEgress -v`
Expected: FAIL — no such rule.

- [ ] **Step 7: Add the env-egress rule**

In `networkpolicy.go`, after the DNS rule and before the resolved-peers loop, add the env-egress rule:
```go
	// Egress rule 2: the agent reaches other env-labeled pods in this
	// namespace -- the dependency/runtime pods the ServiceSet reconciles carry
	// the same env label (serviceset_controller.entryLabels), so the agent can
	// connect to deps via Service DNS and to runtimes by pod IP. A
	// podSelector-only peer (no namespaceSelector) selects pods in THIS
	// NetworkPolicy's namespace. No ports => all ports (the agent needs only the
	// declared service ports, but a single all-ports rule is simpler and the
	// namespace is already the trust boundary).
	egress = append(egress, envPodEgressRule(in.Env.Name))
```
Add the helper:
```go
// envPodEgressRule allows egress to pods in this namespace carrying the
// environment label (the agent + the ServiceSet's dep/runtime pods).
func envPodEgressRule(envName string) *anetworkingv1.NetworkPolicyEgressRuleApplyConfiguration {
	return anetworkingv1.NetworkPolicyEgressRule().
		WithTo(anetworkingv1.NetworkPolicyPeer().
			WithPodSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{
				"sandbox.psenna.dev/environment": EnvironmentLabelValue(envName),
			})))
}
```
(Reorder the egress slice so the env rule is appended before the `sortedPeers` loop, OR insert at a fixed index — the existing code appends DNS then peers; appending env after DNS (index 1) keeps it stable. The test should find the rule by its podSelector, not by index, so ordering does not break it.)

- [ ] **Step 8: Run the NP test + full render suite**

Run: `IN_CONTAINER=1 go test ./internal/render/ -count=1`
Expected: PASS (new test + existing NP tests).

- [ ] **Step 9: Run the whole operator suite**

Run: `IN_CONTAINER=1 KUBEBUILDER_ASSETS=/tmp/envtest/k8s/1.35.0 go test -race -count=1 ./...`
Expected: PASS (all Plan 1 + Plan 2 tests green).

- [ ] **Step 10: Commit**

```bash
git add internal/controller/serviceset_controller.go internal/controller/serviceset_controller_test.go internal/render/networkpolicy.go internal/render/networkpolicy_test.go
git commit -m "feat: dep/runtime pods inherit env NetworkPolicy (env label + no token + env egress)

Refs #24 (Plan 2 of 3; does not close)"
```

---

## Self-Review (run before SDD execution)

**1. Spec coverage (design §1–§6):**
- §1 `services.yaml` declaration + field set → Task 2 (parser) + Task 3 (compose). ✓
- §2 reconcile (agent POSTs → operator upserts ServiceSet CR owned by env → controller reconciles) → Task 5 (apply + upsert) + Plan 1 controller. ✓
- §3 exec (operator execs into runtime pod, streams stdio) → Task 6 (one-shot). ✓ (Interactive streaming deferred — documented scope cut, matches §7's YAGNI posture.)
- §4 security (no hostPath/loopback-only/agent holds no credential/NetworkPolicy isolation) → Task 4 (RBAC) + Task 8 (NP) + Global Constraints. ✓
- §5 workspace/storage (RWX/RWO deferred; marker Destroyed.Pods) → Destroyed.Pods is Plan 3 (e2e); the RWO baseline is inherited. ✓ (Note in Plan 3.)
- §6 test surface (lifecycle, isolation, control-plane isolation, services, compose, snapshot/teardown) → Plan 3 (e2e). This plan's unit/envtest: Tasks 1–8. ✓

**2. Placeholder scan:** No TBD/TODO. Code blocks are complete. Modification steps name the exact file + the exact new code + where it goes. (Two steps instruct the implementer to read an existing test file's style first — that is reading context, not a placeholder, and is bounded: "mirror its style.")

**3. Type consistency:**
- `serviceSetApplier.Upsert(ctx, v1alpha1.ServiceSetSpec) error` — defined Task 5, used by `handlers.sets` (Task 5) — consistent.
- `Execer.Exec(ctx, podName string, cmd []string, stdin []byte) ([]byte, []byte, error)` — defined Task 6, used by `handlers.execer` (Task 6) — consistent.
- `NewServer` signature final shape: `(cfg, store, poll, env, sets serviceSetApplier, execer Execer, now, log)` — Tasks 5 & 6 both widen it; the final signature after Task 6 is what `run.go` calls. Both tasks' server_test updates must reach this shape. (Task 5 adds `sets`; Task 6 adds `execer` after `sets`.)
- `EnvironmentLabelValue` / `render.EnvironmentLabelValue` — used in Task 8 controller + NP; defined in `render/names.go` (existing). Consistent.
- `ValidateServiceSet` / `ParseServicesYAML` / `Compose` — defined Task 2/3, used Task 5 (apply), Task 3 (compose CLI). Consistent.
- Error codes `CodeDuplicateEntryName` etc. — added Task 2 (api.go), used Task 2 (Validate) + Task 5 (handler). Consistent.

**4. Conflict scan:** Task 5 and Task 6 both edit `server.go`/`handlers.go`/`api.go`/`limits.go`/`run.go`/`services_cli.go`/`main.go` — sequential, not parallel (SDD is one implementer at a time), so no merge conflict. Task 7 and Task 8 both edit `serviceset_controller.go` — sequential. The `NewServer` signature grows across two tasks; Task 6 is the final shape. Task 5's server_test edits must not assume `execer` exists (pass nothing / nil is added by Task 6) — Task 5 passes only `sets`; Task 6 adds `execer`. (Each task updates the call sites it touches; the final shape is reached after Task 6.)

No blocking issues. Proceed to SDD execution.