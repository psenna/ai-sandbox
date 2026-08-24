# ServiceSet CRD + Reconcile Controller Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `ServiceSet` CRD and a controller that reconciles declared dependency services and dev-tool runtimes into native k8s Pods/Services/PVCs in the agent's namespace, with per-entry `Ready` status, `dependsOn` ordering, and recreate-on-change.

**Architecture:** A new namespaced CRD `ServiceSet` (group `sandbox.psenna.dev/v1alpha1`, sibling to `SandboxEnvironment`/`SandboxClass`) holds a desired-state list of `services` (long-lived deps, each → Service + Pod, optionally a data PVC) and `runtimes` (long-lived dev-tool pods the agent execs into, mounting the shared workspace PVC). A new `ServiceSetReconciler` (controller-runtime, wired like the existing `SandboxEnvironment` reconciler) reconciles each entry to its child objects, owns them (garbage-collected with the `ServiceSet`), computes readiness from each Pod's `Ready` condition gated by `dependsOn`, and recreates Pods whose pod-affecting spec changed (retaining per-service data PVCs). This is the foundation: it is exercised in envtest by creating a `ServiceSet` CR directly — no agent, no control API, no e2e cluster. Plans 2 and 3 build the agent-facing surface and teardown on top of it.

**Tech Stack:** Go 1.25, controller-runtime v0.23.3, k8s.io/api v0.35.0, controller-tools v0.20.1 (controller-gen), envtest k8s 1.35.0. Tests: `testing` + envtest (real API server + etcd, no kubelet/scheduler/controller-manager — Pod readiness is simulated by patching Pod status in tests).

## Global Constraints

Copied verbatim from the design spec (`docs/superpowers/specs/2026-08-23-k8s-native-execution-model-design.md`) and the codebase conventions the exploration mapped:

- The operator's API group/version is `sandbox.psenna.dev/v1alpha1`; new types go in `operator/api/v1alpha1/` and self-register via `func init() { SchemeBuilder.Register(&T{}, &TList{}) }`. `operator/internal/operator/manager.go:Scheme()` already calls `sandboxv1alpha1.AddToScheme`, so no scheme change is needed.
- Generated artifacts are produced by the existing Make targets and are committed: `make generate` (deepcopy → `operator/api/v1alpha1/zz_generated.deepcopy.go`), `make manifests` (CRD → `operator/config/crd/bases/`, RBAC → `operator/config/rbac/role.yaml`), `make helm-crds` (copy CRDs into `operator/deploy/helm/ai-sandbox-operator/crds/`). A new CRD file must ALSO be appended to the `CRD_FILES` Makefile variable so `make crd-docs` regenerates `operator/docs/crd-reference.md`.
- envtest loads CRDs from `operator/config/crd/bases/` (`operator/internal/controller/suite_test.go` `CRDDirectoryPaths`), so `make manifests` must run before envtest tests see the new CRD.
- **Local (non-Docker) Make invocations:** pass `IN_CONTAINER=1` to EVERY `make` target run locally. The Makefile has a parse-time guard that errors (`CURDIR is not under SHARED_DIR -- set SHARED_DIR=, or run with IN_CONTAINER=1`) for any target when `IN_CONTAINER` is unset, even plain-`cp` targets like `helm-crds`/`helm-crds-check` that do not otherwise use `$(DOCKER_RUN)`. With `IN_CONTAINER=1`, `DOCKER_RUN` is emptied so `generate`, `manifests`, `crd-docs`, `crd-docs-check`, `envtest-assets`, `test-envtest` run with the host Go toolchain, and `helm-crds`/`helm-crds-check` still run as plain `cp`/`diff`. So always write e.g. `make IN_CONTAINER=1 generate`, `make IN_CONTAINER=1 helm-crds`, `make IN_CONTAINER=1 test-envtest`. Run all `make` targets from the `operator/` directory. Plain `go test ./api/v1alpha1/...` (the unit test for the types) runs directly with no Make wrapper.
- Controllers live in `operator/internal/controller/` with package-level `+kubebuilder:rbac:` markers, a `Reconcile(ctx, req) (ctrl.Result, error)` method, and a `SetupWithManager`; wired in `operator/internal/operator/controllers.go:SetupControllers`.
- envtest runs no kube-controller-manager/kubelet: PVCs do not bind and Pods do not run. Tests simulate Pod readiness by patching `pod.Status.Conditions` and PVCs by creating them as fixtures. The controller must read readiness from `pod.Status.Conditions` (PodReady), not from pod `Phase`.
- The agent holds no k8s credential and never creates the `ServiceSet` directly — that is Plan 2's control-API surface. Plan 1 only defines the CRD + controller and tests them via envtest fixtures.
- No hostPath, no `privileged`, no new `securityContext.devices`. The `ServiceSet` controller must not introduce any such field. Per-service data PVCs are `ReadWriteOnce`; runtime pods reference the existing workspace PVC (`<environmentName>-workspace`, rendered by the environment controller as `ReadWriteOnce`) — sharing an RWO PVC across pods works on a single node (both kind and single-node k3s), which is the supported baseline. Multi-node + RWO is documented as a known limitation (out of Plan 1's scope).
- DRY/YAGNI: implement only the fields the design lists (`name, image, ports, env, envFromSecret, command, args, resources, imagePullPolicy, runAsUser, healthcheck, dependsOn` + service-only `storage, expose` + runtime-only `mountWorkspace`). No ephemeral-run path, no compose, no exec, no engine-type change — those are Plans 2/3.

---

## File Structure

**Create:**
- `operator/api/v1alpha1/serviceset_types.go` — the `ServiceSet` CRD types (Spec, Status, `ServiceSpec`, `RuntimeSpec`, `HealthcheckSpec`, `ServiceStorageSpec`, `EntryStatus`, root + list types, `init()` registration, kubebuilder markers).
- `operator/internal/controller/serviceset_controller.go` — `ServiceSetReconciler` (`Reconcile`, `SetupWithManager`, RBAC markers) + unexported reconcile helpers (`reconcileService`, `reconcileRuntime`, `desiredService`, `desiredPod`, `desiredPVC`, `podSpecHash`, `isPodReady`, `computeReady`, `pruneChildren`).
- `operator/internal/controller/serviceset_controller_test.go` — envtest tests for each task (one `Test*` per behavior, using the shared suite helpers).

**Modify (generated — via Make targets, then committed):**
- `operator/api/v1alpha1/zz_generated.deepcopy.go` — `make generate`.
- `operator/config/crd/bases/sandbox.psenna.dev_servicesets.yaml` — `make manifests`.
- `operator/config/rbac/role.yaml` — `make manifests` (adds the ServiceSet RBAC rules from the markers).
- `operator/deploy/helm/ai-sandbox-operator/crds/sandbox.psenna.dev_servicesets.yaml` — `make helm-crds`.
- `operator/docs/crd-reference.md` — `make crd-docs` (after appending to `CRD_FILES`).
- `operator/Makefile` — append `sandbox.psenna.dev_servicesets.yaml` to the `CRD_FILES` variable.
- `operator/internal/operator/controllers.go` — construct + register `ServiceSetReconciler` in `SetupControllers`.

**Reference (read-only, for patterns):**
- `operator/api/v1alpha1/sandboxenvironment_types.go` (root type markers, `init()`, status conditions shape), `operator/api/v1alpha1/sandboxclass_types.go` (`EngineSpec`, `WorkspaceSpec` at line 254 — has `Size` + `StorageClassName`, no access-mode field).
- `operator/internal/controller/sandboxenvironment_controller.go:188` (`Reconcile` signature), `:260` (`SetupWithManager` with `.For(...).Owns(...)`), `:3-19` (RBAC markers), `internal/operator/controllers.go:17` (`SetupControllers`).
- `operator/internal/controller/suite_test.go` (envtest `TestMain`, `k8s` client, `reconcileOnce(t,r,key)` at line 419, `newReconciler`/`newResourceReconciler`).
- `operator/internal/render/names.go:15` (`SuffixWorkspace = "-workspace"`), `names.go:44` (`ChildNames` → `PVC = childName(envName, SuffixWorkspace)`). Workspace PVC name = `<environmentName>-workspace`.
- `operator/internal/render/inputs.go:17` (`WorkspaceMountPath = "/workspace"`), `pod.go:60` (`workspaceVolumeName = "workspace"`).

---

### Task 1: ServiceSet API types + generated artifacts

**Files:**
- Create: `operator/api/v1alpha1/serviceset_types.go`
- Modify: `operator/api/v1alpha1/zz_generated.deepcopy.go` (`make generate`), `operator/config/crd/bases/sandbox.psenna.dev_servicesets.yaml` (`make manifests`), `operator/config/rbac/role.yaml` (`make manifests`), `operator/deploy/helm/ai-sandbox-operator/crds/sandbox.psenna.dev_servicesets.yaml` (`make helm-crds`), `operator/docs/crd-reference.md` (`make crd-docs`), `operator/Makefile` (`CRD_FILES`)
- Test: `operator/api/v1alpha1/serviceset_types_test.go`

**Interfaces:**
- Produces: `ServiceSet`, `ServiceSetList`, `ServiceSetSpec`, `ServiceSetStatus`, `ServiceSpec`, `RuntimeSpec`, `HealthcheckSpec`, `ServiceStorageSpec`, `EntryStatus` (all in package `v1alpha1`), self-registered via `SchemeBuilder.Register(&ServiceSet{}, &ServiceSetList{})`. Later tasks reference these by exact name.

- [ ] **Step 1: Write the types file**

Create `operator/api/v1alpha1/serviceset_types.go`:

```go
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceSetSpec declares the long-lived dependency services and dev-tool
// runtimes for one SandboxEnvironment. The ServiceSetReconciler reconciles each
// entry to native Pods/Services/PVCs in the ServiceSet's namespace.
type ServiceSetSpec struct {
	// environmentName is the SandboxEnvironment this set belongs to. Used to
	// derive the shared workspace PVC name (<environmentName>-workspace) that
	// runtime pods mount.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	EnvironmentName string `json:"environmentName"`

	// services are long-lived dependency pods. Each gets a Service (cluster
	// DNS <name>.<ns>.svc) and, if storage is set, a retained data PVC.
	// +listType=map
	// +listMapKey=name
	Services []ServiceSpec `json:"services,omitempty"`

	// runtimes are long-lived dev-tool pods the agent execs into. They mount the
	// shared workspace PVC and have no Service.
	// +listType=map
	// +listMapKey=name
	Runtimes []RuntimeSpec `json:"runtimes,omitempty"`
}

// ServiceSpec describes one long-lived dependency pod.
type ServiceSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Image string `json:"image"`
	Ports []int32 `json:"ports,omitempty"`
	Env map[string]string `json:"env,omitempty"`
	// envFromSecret references a Secret name whose keys become env vars.
	EnvFromSecret *string `json:"envFromSecret,omitempty"`
	Command []string `json:"command,omitempty"`
	Args []string `json:"args,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	RunAsUser *int64 `json:"runAsUser,omitempty"`
	Healthcheck HealthcheckSpec `json:"healthcheck,omitempty"`
	// dependsOn names other service/runtime entries that must be Ready first.
	DependsOn []string `json:"dependsOn,omitempty"`
	Storage *ServiceStorageSpec `json:"storage,omitempty"`
	// expose, if set, publishes the first port as a NodePort on this host port.
	Expose *int32 `json:"expose,omitempty"`
}

// RuntimeSpec describes one long-lived dev-tool pod the agent execs into.
type RuntimeSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Image string `json:"image"`
	// mountWorkspace mounts the shared workspace PVC at /workspace. Defaults to
	// true when omitted.
	// +kubebuilder:default=true
	MountWorkspace *bool `json:"mountWorkspace,omitempty"`
	Command []string `json:"command,omitempty"`
	Args []string `json:"args,omitempty"`
	Env map[string]string `json:"env,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	RunAsUser *int64 `json:"runAsUser,omitempty"`
	Healthcheck HealthcheckSpec `json:"healthcheck,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

// HealthcheckSpec maps to a k8s readinessProbe. Exactly one of exec/http/tcp.
type HealthcheckSpec struct {
	Exec []string `json:"exec,omitempty"`
	HTTP *HTTPProbe `json:"http,omitempty"`
	TCP *TCPProbe `json:"tcp,omitempty"`
	// interval defaults to 5s when empty.
	Interval string `json:"interval,omitempty"`
}

type HTTPProbe struct {
	Path string `json:"path"`
	Port int32 `json:"port"`
}

type TCPProbe struct {
	Port int32 `json:"port"`
}

// ServiceStorageSpec creates a per-service RWO data PVC, retained by name across
// Pod recreates.
type ServiceStorageSpec struct {
	// size is a quantity string, e.g. "1Gi".
	// +kubebuilder:validation:Required
	Size string `json:"size"`
	// mountPath is the in-container mount point for the data PVC.
	// +kubebuilder:validation:Required
	MountPath string `json:"mountPath"`
}

// EntryStatus is the readiness of one service or runtime entry.
type EntryStatus struct {
	Name string `json:"name"`
	// kind is "service" or "runtime".
	Kind string `json:"kind"`
	Ready bool `json:"ready"`
	Reason string `json:"reason,omitempty"`
}

// ServiceSetStatus is the reconciled state of a ServiceSet.
type ServiceSetStatus struct {
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	Entries []EntryStatus `json:"entries,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sbset,categories=sandbox
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ServiceSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec   ServiceSetSpec   `json:"spec,omitempty"`
	Status ServiceSetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ServiceSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items []ServiceSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceSet{}, &ServiceSetList{})
}
```

- [ ] **Step 2: Write the failing test (JSON round-trip + scheme registration)**

Create `operator/api/v1alpha1/serviceset_types_test.go`:

```go
package v1alpha1

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestServiceSet_RoundTripsJSON(t *testing.T) {
	mount := true
	ss := ServiceSet{
		Spec: ServiceSetSpec{
			EnvironmentName: "env-1",
			Services: []ServiceSpec{{
				Name:  "postgres",
				Image: "postgres:18-alpine",
				Ports: []int32{5432},
				Env:   map[string]string{"POSTGRES_USER": "e2e"},
				Storage: &ServiceStorageSpec{Size: "1Gi", MountPath: "/var/lib/postgresql/data"},
				Healthcheck: HealthcheckSpec{Exec: []string{"pg_isready"}, Interval: "5s"},
			}},
			Runtimes: []RuntimeSpec{{
				Name:           "python",
				Image:          "python:3.13-slim",
				MountWorkspace: &mount,
				Command:        []string{"sleep", "infinity"},
			}},
		},
	}
	b, err := json.Marshal(ss)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ServiceSet
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Spec.Services[0].Name != "postgres" || got.Spec.Runtimes[0].Image != "python:3.13-slim" {
		t.Fatalf("round-trip lost fields: %+v", got.Spec)
	}
}

// TestServiceSet_RegisteredInScheme proves the init() block wired it in so
// AddToScheme picks it up (manager.go:Scheme needs no change).
func TestServiceSet_RegisteredInScheme(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if !scheme.Recognizes(GroupVersion.WithKind("ServiceSet")) {
		t.Fatal("ServiceSet not registered; check the init() in serviceset_types.go")
	}
}
```

Run: `cd operator && go test ./api/v1alpha1/ -run TestServiceSet -v`
Expected: FAIL — `undefined: ServiceSet` (types file not yet compiled in) until the file is present, then PASS once compiled. Actually the file is written in Step 1, so this passes immediately; the test exists to lock the JSON tags and registration. Run it and confirm PASS.

- [ ] **Step 3: Generate deepcopy + CRD + RBAC**

```sh
cd operator && make IN_CONTAINER=1 generate && make IN_CONTAINER=1 manifests
```

Verify the generated artifacts exist:
```sh
test -f api/v1alpha1/zz_generated.deepcopy.go && grep -q "func (in \*ServiceSet) DeepCopy" api/v1alpha1/zz_generated.deepcopy.go
test -f config/crd/bases/sandbox.psenna.dev_servicesets.yaml
```
(There is no `servicesets` RBAC yet — RBAC rules are generated from `+kubebuilder:rbac` markers on the `ServiceSetReconciler`, which does not exist until Task 2. `make manifests` regenerates `config/rbac/role.yaml` but leaves it byte-identical at Task 1. The `servicesets` RBAC verification happens in Task 2.)

- [ ] **Step 4: Copy CRD into the Helm chart + regenerate CRD docs**

Append the new CRD filename to `CRD_FILES` in `operator/Makefile` (the assignment at Makefile:315 is `CRD_FILES ?= config/crd/bases/sandbox.psenna.dev_sandboxclasses.yaml \ config/crd/bases/sandbox.psenna.dev_sandboxenvironments.yaml` — append ` \\\n             config/crd/bases/sandbox.psenna.dev_servicesets.yaml` to it). Then:

```sh
cd operator && make IN_CONTAINER=1 helm-crds && make IN_CONTAINER=1 crd-docs
```

Verify:
```sh
test -f deploy/helm/ai-sandbox-operator/crds/sandbox.psenna.dev_servicesets.yaml
grep -q "ServiceSet" docs/crd-reference.md
```

Run `make helm-crds-check` to confirm no drift:
```sh
cd operator && make IN_CONTAINER=1 helm-crds-check
```

- [ ] **Step 5: Commit**

```sh
git add api/v1alpha1/serviceset_types.go api/v1alpha1/serviceset_types_test.go \
  api/v1alpha1/zz_generated.deepcopy.go config/crd/bases/sandbox.psenna.dev_servicesets.yaml \
  deploy/helm/ai-sandbox-operator/crds/sandbox.psenna.dev_servicesets.yaml \
  docs/crd-reference.md Makefile
git commit -m "feat(api): add ServiceSet CRD types for declared services/runtimes"
```
(Do NOT add `config/rbac/role.yaml` — `make manifests` regenerates it but leaves it byte-identical at Task 1, so there is no RBAC change to commit. The `servicesets` RBAC rules appear in Task 2 once the `ServiceSetReconciler` and its `+kubebuilder:rbac` markers exist.)

---

### Task 2: Controller skeleton + wiring (reconcile is a no-op)

**Files:**
- Create: `operator/internal/controller/serviceset_controller.go`, `operator/internal/controller/serviceset_controller_test.go`
- Modify: `operator/internal/operator/controllers.go`

**Interfaces:**
- Consumes: `v1alpha1.ServiceSet`, `v1alpha1.ServiceSetSpec` (Task 1).
- Produces: `ServiceSetReconciler` with `Reconcile(ctx, req) (ctrl.Result, error)` and `SetupWithManager(mgr) error`; registered in `SetupControllers`. The reconciler for this task fetches the `ServiceSet` and returns `nil` (no-op); later tasks fill the body.

- [ ] **Step 1: Write the controller skeleton**

Create `operator/internal/controller/serviceset_controller.go`:

```go
package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=servicesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=servicesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=servicesets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

// ServiceSetReconciler reconciles a ServiceSet to native Pods/Services/PVCs.
type ServiceSetReconciler struct {
	client.Client
}

func (r *ServiceSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ss sandboxv1alpha1.ServiceSet
	if err := r.Get(ctx, req.NamespacedName, &ss); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Filled in by later tasks: reconcile services, runtimes, readiness, prune.
	_ = ss
	return ctrl.Result{}, nil
}

func (r *ServiceSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.ServiceSet{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Named("serviceset").
		Complete(r)
}
```

Add the `corev1` import (`corev1 "k8s.io/api/core/v1"`).

- [ ] **Step 2: Wire it into SetupControllers**

In `operator/internal/operator/controllers.go`, inside `SetupControllers(mgr, cfg)` (the function at line 17 that constructs+registers each reconciler), add after the existing reconciler wiring:

```go
if err := (&controller.ServiceSetReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
	return fmt.Errorf("serviceset controller: %w", err)
}
```

(Match the exact construction style of the existing reconciler in that file — read it first and mirror the error-wrap convention.)

- [ ] **Step 3: Regenerate RBAC and write the failing test**

```sh
cd operator && make IN_CONTAINER=1 manifests
```

Now the `+kubebuilder:rbac:...,resources=servicesets,...` markers on `ServiceSetReconciler` generate real RBAC rules. Verify them (this is the check deferred from Task 1):
```sh
grep -q "servicesets" config/rbac/role.yaml && grep -q "pods" config/rbac/role.yaml
```

Create `operator/internal/controller/serviceset_controller_test.go`:

```go
package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func newServiceSetReconciler(t *testing.T) *ServiceSetReconciler {
	t.Helper()
	return &ServiceSetReconciler{Client: k8s}
}

func mustCreateServiceSet(t *testing.T, ss *sandboxv1alpha1.ServiceSet) *sandboxv1alpha1.ServiceSet {
	t.Helper()
	if err := k8s.Create(ctx, ss); err != nil {
		t.Fatalf("create ServiceSet: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, ss) })
	return ss
}

func TestServiceSetReconciler_NoOpFetchesAndReturns(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{
		Spec: sandboxv1alpha1.ServiceSetSpec{EnvironmentName: "env-x"},
	}
	ss.Name, ss.Namespace = "set-x", "default"
	mustCreateServiceSet(t, ss)

	r := newServiceSetReconciler(t)
	if _, err := reconcileServiceSetOnce(t, r, types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}
```

Add a thin `reconcileServiceSetOnce` helper next to the existing `reconcileOnce` (suite_test.go:419) — or reuse `reconcileOnce` if its signature (`r.Reconcile`-calling) is generic enough. Check `reconcileOnce`'s signature first; if it is typed to the environment `Reconciler`, add:

```go
func reconcileServiceSetOnce(t *testing.T, r *ServiceSetReconciler, key types.NamespacedName) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
}
```

(`ctx` is the package-level test context already used by `reconcileOnce`; confirm by reading `suite_test.go`.)

- [ ] **Step 4: Run envtest**

```sh
cd operator && make IN_CONTAINER=1 envtest-assets && make IN_CONTAINER=1 test-envtest 2>&1 | grep -E "ServiceSet|serviceset|FAIL|ok"
```
Expected: the new test PASSES (reconcile fetches the CR and returns nil). The CRD is visible because Task 1 generated it into `config/crd/bases`.

If envtest fails to find the CRD, confirm `config/crd/bases/sandbox.psenna.dev_servicesets.yaml` exists (Task 1 Step 3).

- [ ] **Step 5: Commit**

```sh
git add internal/controller/serviceset_controller.go internal/controller/serviceset_controller_test.go \
  internal/operator/controllers.go config/rbac/role.yaml
git commit -m "feat(controller): add ServiceSetReconciler skeleton wired into SetupControllers"
```

---

### Task 3: Reconcile a service → Service + Pod (+ data PVC)

**Files:**
- Modify: `operator/internal/controller/serviceset_controller.go` (fill `Reconcile`, add `reconcileService`, `desiredService`, `desiredPod`, `desiredPVC`, `entryLabels`, `podSpecHash`)
- Test: `operator/internal/controller/serviceset_controller_test.go`

**Interfaces:**
- Produces: for each `ServiceSpec`, the reconciler ensures a `Service/<name>` (ClusterIP, ports, selector), a `Pod/<name>` (image/env/command/readinessProbe/labels/spec-hash annotation), and if `Storage` set a `PVC/<name>-data` (RWO, size), all owned by the `ServiceSet`. `podSpecHash(spec) string` is the recreate-detection key used again in Task 6.

- [ ] **Step 1: Write the failing test**

Append to `serviceset_controller_test.go`:

```go
func TestServiceSetReconciler_CreatesServicePodAndPVC(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-svc",
		Services: []sandboxv1alpha1.ServiceSpec{{
			Name:   "postgres",
			Image:  "postgres:18-alpine",
			Ports:  []int32{5432},
			Env:    map[string]string{"POSTGRES_USER": "e2e", "POSTGRES_PASSWORD": "e2e"},
			Storage: &sandboxv1alpha1.ServiceStorageSpec{Size: "1Gi", MountPath: "/var/lib/postgresql/data"},
			Healthcheck: sandboxv1alpha1.HealthcheckSpec{Exec: []string{"pg_isready", "-U", "e2e"}, Interval: "5s"},
		}},
	}}
	ss.Name, ss.Namespace = "set-svc", "default"
	mustCreateServiceSet(t, ss)

	r := newServiceSetReconciler(t)
	key := types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}
	if _, err := reconcileServiceSetOnce(t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Pod created with image, env, readinessProbe, data-PVC mount, spec-hash label.
	var pod corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Name: "postgres", Namespace: "default"}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Spec.Containers[0].Image != "postgres:18-alpine" {
		t.Fatalf("pod image = %q", pod.Spec.Containers[0].Image)
	}
	if pod.Spec.Containers[0].ReadinessProbe == nil || pod.Spec.Containers[0].ReadinessProbe.Exec == nil {
		t.Fatal("pod missing exec readinessProbe from healthcheck")
	}
	if got := envValue(pod, "POSTGRES_USER"); got != "e2e" {
		t.Fatalf("POSTGRES_USER env = %q", got)
	}
	if !hasVolumeMount(pod, "postgres-data", "/var/lib/postgresql/data") {
		t.Fatal("pod missing data PVC mount postgres-data at /var/lib/postgresql/data")
	}
	if pod.Annotations["ai-sandbox.io/spec-hash"] == "" {
		t.Fatal("pod missing ai-sandbox.io/spec-hash annotation")
	}
	if !ownedBy(&pod, ss) {
		t.Fatal("pod not owned by ServiceSet")
	}

	// Service created with port 5432 and selector matching the pod labels.
	var svc corev1.Service
	if err := k8s.Get(ctx, types.NamespacedName{Name: "postgres", Namespace: "default"}, &svc); err != nil {
		t.Fatalf("get svc: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 5432 {
		t.Fatalf("svc ports = %+v", svc.Spec.Ports)
	}
	if !ownedBy(&svc, ss) {
		t.Fatal("svc not owned by ServiceSet")
	}

	// Data PVC created, RWO, size 1Gi, owned by ServiceSet.
	var pvc corev1.PersistentVolumeClaim
	if err := k8s.Get(ctx, types.NamespacedName{Name: "postgres-data", Namespace: "default"}, &pvc); err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("pvc accessmodes = %+v", pvc.Spec.AccessModes)
	}
	if pvc.Spec.Resources.Requests.Storage().String() != "1Gi" {
		t.Fatalf("pvc size = %q", pvc.Spec.Resources.Requests.Storage())
	}
	if !ownedBy(&pvc, ss) {
		t.Fatal("pvc not owned by ServiceSet")
	}
}
```

Add the small helpers (`envValue`, `hasVolumeMount`, `ownedBy`) to the test file:

```go
func envValue(pod corev1.Pod, name string) string {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
func hasVolumeMount(pod corev1.Pod, name, path string) bool {
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == name && vm.MountPath == path {
			return true
		}
	}
	return false
}
func ownedBy(obj client.Object, owner *sandboxv1alpha1.ServiceSet) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "ServiceSet" && ref.Name == owner.Name && ref.UID == owner.UID {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run the test to verify it fails**

```sh
cd operator && make IN_CONTAINER=1 envtest-assets && KUBEBUILDER_ASSETS=$(...go run ...) go test ./internal/controller/ -run TestServiceSetReconciler_CreatesServicePodAndPVC -v
```
Expected: FAIL — no Pod/Service/PVC created (reconcile is still a no-op).

- [ ] **Step 3: Implement the reconcile body + helpers**

Fill `Reconcile` in `serviceset_controller.go` and add the helpers. Use the plain controller-runtime client (Get/Create/Update/Delete) — not server-side apply — because Pods must be **recreated** when their pod-affecting spec changes (Task 6), and SSA cannot change an immutable Pod spec.

```go
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const (
	// specHashAnnotation records the hash of the pod-affecting spec; a mismatch
	// triggers Pod recreation (Task 6).
	specHashAnnotation = "ai-sandbox.io/spec-hash"
	// owner labels identify a ServiceSet's children (used by prune + Service selector).
	labelServiceset = "ai-sandbox.io/serviceset"
	labelEntry      = "ai-sandbox.io/entry"
	labelKind       = "ai-sandbox.io/kind"
)

func (r *ServiceSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ss sandboxv1alpha1.ServiceSet
	if err := r.Get(ctx, req.NamespacedName, &ss); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	for i := range ss.Spec.Services {
		if err := r.reconcileService(ctx, &ss, &ss.Spec.Services[i]); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Runtimes are reconciled in Task 4.
	return ctrl.Result{}, nil
}

func (r *ServiceSetReconciler) reconcileService(ctx context.Context, ss *sandboxv1alpha1.ServiceSet, s *sandboxv1alpha1.ServiceSpec) error {
	labels := entryLabels(ss, s.Name, "service")
	if s.Storage != nil {
		if err := r.ensurePVC(ctx, ss, s.Name+"-data", s.Storage.Size, labels); err != nil {
			return fmt.Errorf("pvc %s: %w", s.Name, err)
		}
	}
	if err := r.ensureService(ctx, ss, s, labels); err != nil {
		return fmt.Errorf("service %s: %w", s.Name, err)
	}
	if err := r.ensurePod(ctx, ss, s.Name, s.Image, s.Command, s.Args, s.Env, s.EnvFromSecret,
		s.Resources, s.ImagePullPolicy, s.RunAsUser, s.Healthcheck, labels, serviceVolumeMounts(s), serviceEnvFrom(s)); err != nil {
		return fmt.Errorf("pod %s: %w", s.Name, err)
	}
	return nil
}

func entryLabels(ss *sandboxv1alpha1.ServiceSet, name, kind string) map[string]string {
	return map[string]string{
		labelServiceset: ss.Name,
		labelEntry:       name,
		labelKind:        kind,
	}
}

func serviceVolumeMounts(s *sandboxv1alpha1.ServiceSpec) []corev1.VolumeMount {
	if s.Storage == nil {
		return nil
	}
	return []corev1.VolumeMount{{Name: s.Name + "-data", MountPath: s.Storage.MountPath}}
}

func serviceEnvFrom(s *sandboxv1alpha1.ServiceSpec) *corev1.EnvFromSource {
	if s.EnvFromSecret == nil {
		return nil
	}
	return &corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: *s.EnvFromSecret}}}
}

func (r *ServiceSetReconciler) ensurePVC(ctx context.Context, ss *sandboxv1alpha1.ServiceSet, name, size string, labels map[string]string) error {
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ss.Namespace}, &existing)
	if err == nil {
		return nil // retained by name across recreates; do not mutate (Task 6 may grow size, left out of Plan 1 scope)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ss.Namespace, Labels: labels, OwnerReferences: []metav1.OwnerReference{ownerRef(ss)}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:  pvcVolumeMode(),
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
		},
	}
	return r.Create(ctx, pvc)
}

func pvcVolumeMode() *corev1.PersistentVolumeMode {
	m := corev1.PersistentVolumeFilesystem
	return &m
}

func (r *ServiceSetReconciler) ensureService(ctx context.Context, ss *sandboxv1alpha1.ServiceSet, s *sandboxv1alpha1.ServiceSpec, labels map[string]string) error {
	// A Service with no ports has nothing to expose; skip creation (a portless
	// ClusterIP Service is rejected by the API server: spec.ports: Required value).
	// `Ports` is optional in the CRD (no validation marker), so a portless
	// ServiceSpec is valid input — the Pod is still created by reconcileService.
	if len(s.Ports) == 0 {
		return nil
	}
	var existing corev1.Service
	key := types.NamespacedName{Name: s.Name, Namespace: ss.Namespace}
	err := r.Get(ctx, key, &existing)
	svcType := corev1.ServiceTypeClusterIP
	var nodePort int32
	if s.Expose != nil && len(s.Ports) > 0 {
		svcType = corev1.ServiceTypeNodePort
		nodePort = *s.Expose
	}
	desiredPorts := make([]corev1.ServicePort, 0, len(s.Ports))
	for i, p := range s.Ports {
		sp := corev1.ServicePort{Name: fmt.Sprintf("p%d", i), Port: p, TargetPort: intstr.FromInt32(p)}
		if nodePort != 0 && i == 0 {
			sp.NodePort = nodePort
		}
		desiredPorts = append(desiredPorts, sp)
	}
	if err == nil {
		existing.Spec.Ports = desiredPorts
		existing.Spec.Type = svcType
		existing.Spec.Selector = map[string]string{labelServiceset: ss.Name, labelEntry: s.Name}
		if existing.Labels == nil {
			existing.Labels = labels
		}
		return r.Update(ctx, &existing)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: ss.Namespace, Labels: labels, OwnerReferences: []metav1.OwnerReference{ownerRef(ss)}},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Ports:    desiredPorts,
			Selector: map[string]string{labelServiceset: ss.Name, labelEntry: s.Name},
		},
	}
	return r.Create(ctx, svc)
}

func (r *ServiceSetReconciler) ensurePod(ctx context.Context, ss *sandboxv1alpha1.ServiceSet, name, image string, command, args []string, env map[string]string, envFromSecret *string, resources corev1.ResourceRequirements, pullPolicy corev1.PullPolicy, runAsUser *int64, hc sandboxv1alpha1.HealthcheckSpec, labels map[string]string, mounts []corev1.VolumeMount, envFrom *corev1.EnvFromSource) error {
	hash := podSpecHash(image, command, args, env, envFromSecret, resources, pullPolicy, runAsUser, hc, mounts)
	key := types.NamespacedName{Name: name, Namespace: ss.Namespace}
	var existing corev1.Pod
	err := r.Get(ctx, key, &existing)
	if err == nil {
		if existing.GetAnnotations()[specHashAnnotation] == hash {
			return nil // unchanged
		}
		// Changed pod-affecting spec: recreate (Task 6). PVC retained (separate object).
		if err := r.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ss.Namespace, Labels: labels,
			Annotations:      map[string]string{specHashAnnotation: hash},
			OwnerReferences: []metav1.OwnerReference{ownerRef(ss)}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:           name,
				Image:          image,
				Command:        command,
				Args:           args,
				Env:            toEnvVars(env),
				EnvFrom:        envFromSlice(envFrom),
				Resources:      resources,
				ImagePullPolicy: pullPolicy,
				VolumeMounts:   mounts,
				ReadinessProbe: readinessProbe(hc),
			}},
			Volumes: podVolumes(mounts),
		},
	}
	applySecurityContext(pod, runAsUser)
	return r.Create(ctx, pod)
}

// podVolumes turns each volume mount into a PVC-backed volume. Every mount in
// this plan references a PVC by name (a service data PVC "<name>-data", or the
// shared workspace PVC "<environmentName>-workspace"), so each volume's claim
// name equals its mount name.
func podVolumes(mounts []corev1.VolumeMount) []corev1.Volume {
	vols := make([]corev1.Volume, 0, len(mounts))
	for _, m := range mounts {
		vols = append(vols, corev1.Volume{Name: m.Name, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: m.Name}}})
	}
	return vols
}

func applySecurityContext(pod *corev1.Pod, runAsUser *int64) {
	if runAsUser == nil {
		return
	}
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: runAsUser}
}

func toEnvVars(env map[string]string) []corev1.EnvVar {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(env))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: env[k]})
	}
	return out
}

func envFromSlice(e *corev1.EnvFromSource) []corev1.EnvFromSource {
	if e == nil {
		return nil
	}
	return []corev1.EnvFromSource{*e}
}

func readinessProbe(hc sandboxv1alpha1.HealthcheckSpec) *corev1.Probe {
	interval := 5 * time.Second
	if hc.Interval != "" {
		if d, err := time.ParseDuration(hc.Interval); err == nil {
			interval = d
		}
	}
	p := &corev1.Probe{PeriodSeconds: int32(interval.Seconds()), TimeoutSeconds: 2}
	switch {
	case len(hc.Exec) > 0:
		p.ProbeHandler = corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: hc.Exec}}
	case hc.HTTP != nil:
		p.ProbeHandler = corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: hc.HTTP.Path, Port: intstr.FromInt32(hc.HTTP.Port)}}
	case hc.TCP != nil:
		p.ProbeHandler = corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(hc.TCP.Port)}}
	default:
		return nil
	}
	return p
}

func ownerRef(ss *sandboxv1alpha1.ServiceSet) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: sandboxv1alpha1.GroupVersion.String(), Kind: "ServiceSet", Name: ss.Name, UID: ss.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}
}

// podSpecHash is the recreate-detection key: any change to a pod-affecting
// field yields a different hash, so ensurePod deletes+recreates the Pod.
func podSpecHash(image string, command, args []string, env map[string]string, envFromSecret *string, resources corev1.ResourceRequirements, pullPolicy corev1.PullPolicy, runAsUser *int64, hc sandboxv1alpha1.HealthcheckSpec, mounts []corev1.VolumeMount) string {
	type spec struct {
		Image     string
		Command   []string
		Args      []string
		Env       map[string]string
		EnvFrom   *string
		Resources corev1.ResourceRequirements
		Pull      corev1.PullPolicy
		RunAsUser *int64
		HC        sandboxv1alpha1.HealthcheckSpec
		Mounts    []corev1.VolumeMount
	}
	b, _ := json.Marshal(spec{image, command, args, env, envFromSecret, resources, pullPolicy, runAsUser, hc, mounts})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4: Run the test to verify it passes**

```sh
cd operator && go test ./internal/controller/ -run TestServiceSetReconciler_CreatesServicePodAndPVC -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/controller/serviceset_controller.go internal/controller/serviceset_controller_test.go
git commit -m "feat(controller): reconcile ServiceSet services to Pod+Service+PVC"
```

---

### Task 4: Reconcile a runtime → Pod with the shared workspace PVC

**Files:**
- Modify: `operator/internal/controller/serviceset_controller.go` (add `reconcileRuntime`; call it in `Reconcile`)
- Test: `operator/internal/controller/serviceset_controller_test.go`

**Interfaces:**
- Produces: for each `RuntimeSpec`, a `Pod/<name>` with command defaulting to `["sleep","infinity"]`, the shared workspace PVC (`<environmentName>-workspace`) mounted at `/workspace` when `mountWorkspace != false`, owned by the `ServiceSet`. Reuses `ensurePod`/`podSpecHash` from Task 3.

- [ ] **Step 1: Write the failing test**

```go
func TestServiceSetReconciler_CreatesRuntimePodWithWorkspace(t *testing.T) {
	// The workspace PVC is created by the environment controller in production;
	// in envtest pre-create it as a fixture so the runtime pod's volume resolves.
	ws := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "env-rt-workspace", Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	if err := k8s.Create(ctx, ws); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create workspace pvc: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, ws) })

	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-rt",
		Runtimes: []sandboxv1alpha1.RuntimeSpec{{
			Name:    "python",
			Image:   "python:3.13-slim",
			Command: []string{"sleep", "infinity"},
		}},
	}}
	ss.Name, ss.Namespace = "set-rt", "default"
	mustCreateServiceSet(t, ss)

	r := newServiceSetReconciler(t)
	if _, err := reconcileServiceSetOnce(t, r, types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var pod corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Name: "python", Namespace: "default"}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if !hasVolumeMount(pod, "env-rt-workspace", "/workspace") {
		t.Fatal("runtime pod missing workspace PVC mount env-rt-workspace at /workspace")
	}
	if pod.Spec.Containers[0].Image != "python:3.13-slim" {
		t.Fatalf("runtime image = %q", pod.Spec.Containers[0].Image)
	}
	if len(pod.Spec.Containers[0].Command) == 0 || pod.Spec.Containers[0].Command[0] != "sleep" {
		t.Fatalf("runtime command = %+v", pod.Spec.Containers[0].Command)
	}
	if !ownedBy(&pod, ss) {
		t.Fatal("runtime pod not owned by ServiceSet")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Expected: FAIL — no runtime pod created (`Reconcile` only does services).

- [ ] **Step 3: Implement reconcileRuntime**

In `serviceset_controller.go`, add the call in `Reconcile` and the helper:

```go
// in Reconcile, after the services loop:
for i := range ss.Spec.Runtimes {
	if err := r.reconcileRuntime(ctx, &ss, &ss.Spec.Runtimes[i]); err != nil {
		return ctrl.Result{}, err
	}
}

func (r *ServiceSetReconciler) reconcileRuntime(ctx context.Context, ss *sandboxv1alpha1.ServiceSet, rt *sandboxv1alpha1.RuntimeSpec) error {
	labels := entryLabels(ss, rt.Name, "runtime")
	mount := true
	if rt.MountWorkspace != nil {
		mount = *rt.MountWorkspace
	}
	command := rt.Command
	if len(command) == 0 {
		command = []string{"sleep", "infinity"}
	}
	var mounts []corev1.VolumeMount
	if mount {
		mounts = []corev1.VolumeMount{{Name: ss.Spec.EnvironmentName + "-workspace", MountPath: "/workspace"}}
	}
	return r.ensurePod(ctx, ss, rt.Name, rt.Image, command, rt.Args, rt.Env, nil,
		rt.Resources, "", rt.RunAsUser, rt.Healthcheck, labels, mounts, nil)
}
```

(For runtimes there is no `imagePullPolicy` field in `RuntimeSpec` per the design's YAGNI cut; pass empty so k8s applies its default. If a later task adds the field, wire it here.)

- [ ] **Step 4: Run the test to verify it passes**

Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/controller/serviceset_controller.go internal/controller/serviceset_controller_test.go
git commit -m "feat(controller): reconcile ServiceSet runtimes to Pod with workspace PVC"
```

---

### Task 5: Ready status — per-entry Ready from pod readiness, gated by dependsOn

**Files:**
- Modify: `operator/internal/controller/serviceset_controller.go` (add `isPodReady`, `computeReady`, `writeStatus`; call after reconciling entries)
- Test: `operator/internal/controller/serviceset_controller_test.go`

**Interfaces:**
- Produces: `ServiceSet.Status.Entries []EntryStatus` and a `Ready` metav1.Condition. An entry is `Ready` when its Pod's `PodReady` condition is `True` AND every name in its `dependsOn` is itself `Ready`. The aggregate `Ready` condition is `True` when all entries are `Ready`, else `False`/`DependenciesNotReady` or `PodNotReady`.

- [ ] **Step 1: Write the failing test**

```go
func TestServiceSetReconciler_ReadyGatedByDependsOn(t *testing.T) {
	// A depends on B; B depends on nothing. Mark only B's pod Ready first.
	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-deps",
		Services: []sandboxv1alpha1.ServiceSpec{
			{Name: "b", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}},
			{Name: "a", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}, DependsOn: []string{"b"}},
		},
	}}
	ss.Name, ss.Namespace = "set-deps", "default"
	mustCreateServiceSet(t, ss)
	r := newServiceSetReconciler(t)
	key := types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}
	reconcileServiceSetOnce(t, r, key)

	// Nothing ready yet: A not ready (pod not ready AND dep b not ready).
	if _, err := reconcileServiceSetOnce(t, r, key); err != nil {
		t.Fatal(err)
	}
	assertEntryReady(t, key, "a", false)
	assertEntryReady(t, key, "b", false)
	assertReadyCondition(t, key, metav1.ConditionFalse)

	// Mark B's pod Ready: B becomes ready, A still not ready (its own pod not ready).
	markPodReady(t, "b", "default")
	if _, err := reconcileServiceSetOnce(t, r, key); err != nil {
		t.Fatal(err)
	}
	assertEntryReady(t, key, "b", true)
	assertEntryReady(t, key, "a", false)

	// Mark A's pod Ready too: now A ready (dep b ready AND own pod ready), aggregate Ready=True.
	markPodReady(t, "a", "default")
	if _, err := reconcileServiceSetOnce(t, r, key); err != nil {
		t.Fatal(err)
	}
	assertEntryReady(t, key, "a", true)
	assertReadyCondition(t, key, metav1.ConditionTrue)
}
```

Add the helpers:

```go
func markPodReady(t *testing.T, name, ns string) {
	t.Helper()
	var pod corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &pod); err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now(),
	})
	if err := k8s.Status().Update(ctx, &pod); err != nil {
		t.Fatalf("status update pod %s: %v", name, err)
	}
}
func assertEntryReady(t *testing.T, key types.NamespacedName, name string, want bool) {
	t.Helper()
	var ss sandboxv1alpha1.ServiceSet
	if err := k8s.Get(ctx, key, &ss); err != nil {
		t.Fatal(err)
	}
	for _, e := range ss.Status.Entries {
		if e.Name == name {
			if e.Ready != want {
				t.Fatalf("entry %s ready = %v, want %v", name, e.Ready, want)
			}
			return
		}
	}
	t.Fatalf("entry %s not found in status", name)
}
func assertReadyCondition(t *testing.T, key types.NamespacedName, want metav1.ConditionStatus) {
	t.Helper()
	var ss sandboxv1alpha1.ServiceSet
	if err := k8s.Get(ctx, key, &ss); err != nil {
		t.Fatal(err)
	}
	c := apimeta.FindStatusCondition(ss.Status.Conditions, "Ready")
	if c == nil {
		t.Fatal("Ready condition missing")
	}
	if c.Status != want {
		t.Fatalf("Ready condition = %s, want %s (reason=%s)", c.Status, want, c.Reason)
	}
}
```

(Add imports: `apierrors`, `apimeta "k8s.io/apimachinery/pkg/api/meta"`.)

- [ ] **Step 2: Run the test to verify it fails**

Expected: FAIL — `Ready` condition missing / entries empty.

- [ ] **Step 3: Implement readiness + status**

In `serviceset_controller.go`, after reconciling services+runtimes in `Reconcile`, add:

```go
	if err := r.writeStatus(ctx, &ss); err != nil {
		return ctrl.Result{}, err
	}
```

```go
func (r *ServiceSetReconciler) writeStatus(ctx context.Context, ss *sandboxv1alpha1.ServiceSet) error {
	// Capture the live object's status before mutation so the patch sends only
	// the status diff (not spec/metadata), avoiding resourceVersion churn.
	base := ss.DeepCopy()
	ready := r.computeReady(ctx, ss)
	entries := make([]sandboxv1alpha1.EntryStatus, 0, len(ss.Spec.Services)+len(ss.Spec.Runtimes))
	allReady := true
	for _, s := range ss.Spec.Services {
		ok, reason := ready(s.Name)
		entries = append(entries, sandboxv1alpha1.EntryStatus{Name: s.Name, Kind: "service", Ready: ok, Reason: reason})
		if !ok {
			allReady = false
		}
	}
	for _, rt := range ss.Spec.Runtimes {
		ok, reason := ready(rt.Name)
		entries = append(entries, sandboxv1alpha1.EntryStatus{Name: rt.Name, Kind: "runtime", Ready: ok, Reason: reason})
		if !ok {
			allReady = false
		}
	}
	cond := metav1.Condition{Type: "Ready", LastTransitionTime: metav1.Now(), ObservedGeneration: ss.Generation}
	if allReady {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "AllEntriesReady"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "EntriesNotReady"
	}
	apimeta.SetStatusCondition(&ss.Status.Conditions, cond)
	ss.Status.Entries = entries
	return r.Status().Patch(ctx, ss, client.MergeFrom(base))
}

// readyMap is a closure that resolves an entry's readiness (pod ready + deps ready).
type readyMap func(name string) (bool, string)

func (r *ServiceSetReconciler) computeReady(ctx context.Context, ss *sandboxv1alpha1.ServiceSet) readyMap {
	// first pass: pod readiness by name (independent of deps)
	podReady := map[string]bool{}
	for _, s := range ss.Spec.Services {
		podReady[s.Name] = r.isPodReady(ctx, ss.Namespace, s.Name)
	}
	for _, rt := range ss.Spec.Runtimes {
		podReady[rt.Name] = r.isPodReady(ctx, ss.Namespace, rt.Name)
	}
	depMap := map[string][]string{}
	for _, s := range ss.Spec.Services {
		depMap[s.Name] = s.DependsOn
	}
	for _, rt := range ss.Spec.Runtimes {
		depMap[rt.Name] = rt.DependsOn
	}
	var resolve readyMap
	resolve = func(name string) (bool, string) {
		if !podReady[name] {
			return false, "PodNotReady"
		}
		for _, dep := range depMap[name] {
			if ok, _ := resolve(dep); !ok {
				return false, "DependenciesNotReady"
			}
		}
		return true, ""
	}
	return resolve
}

func (r *ServiceSetReconciler) isPodReady(ctx context.Context, ns, name string) bool {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &pod); err != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run the test to verify it passes**

Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/controller/serviceset_controller.go internal/controller/serviceset_controller_test.go
git commit -m "feat(controller): compute ServiceSet readiness gated by dependsOn"
```

---

### Task 6: Recreate-on-change (image/env change recreates the Pod; PVC retained)

**Files:**
- Modify: `operator/internal/controller/serviceset_controller_test.go` (test only — `ensurePod` from Task 3 already deletes+recreates on hash mismatch)
- Test: `operator/internal/controller/serviceset_controller_test.go`

**Interfaces:**
- Validates: `ensurePod`'s spec-hash path. A second reconcile with a changed `image` (or env/command) deletes the old Pod and creates a new one (new UID); a per-service data PVC is retained (same UID) across the recreate. The PVC is a separate object owned by the ServiceSet, not by the Pod, so Pod deletion never touches it.

- [ ] **Step 1: Write the failing test**

```go
func TestServiceSetReconciler_ImageChangeRecreatesPodRetainsPVC(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-recreate",
		Services: []sandboxv1alpha1.ServiceSpec{{
			Name: "python", Image: "python:3.11-slim",
			Storage: &sandboxv1alpha1.ServiceStorageSpec{Size: "1Gi", MountPath: "/data"},
		}},
	}}
	ss.Name, ss.Namespace = "set-rec", "default"
	mustCreateServiceSet(t, ss)
	r := newServiceSetReconciler(t)
	key := types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}
	reconcileServiceSetOnce(t, r, key)

	var podBefore corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Name: "python", Namespace: "default"}, &podBefore); err != nil {
		t.Fatal(err)
	}
	var pvcBefore corev1.PersistentVolumeClaim
	if err := k8s.Get(ctx, types.NamespacedName{Name: "python-data", Namespace: "default"}, &pvcBefore); err != nil {
		t.Fatal(err)
	}

	// Change the image and re-reconcile. Re-fetch first: the reconcile above
	// wrote .status (Task 5 writeStatus), bumping ss.resourceVersion, so an
	// Update with the stale in-memory object would 409-conflict
	// ("the object has been modified"). Get picks up the current RV.
	if err := k8s.Get(ctx, key, ss); err != nil {
		t.Fatal(err)
	}
	ss.Spec.Services[0].Image = "python:3.13-slim"
	if err := k8s.Update(ctx, ss); err != nil {
		t.Fatal(err)
	}
	reconcileServiceSetOnce(t, r, key)

	var podAfter corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Name: "python", Namespace: "default"}, &podAfter); err != nil {
		t.Fatal(err)
	}
	if podAfter.UID == podBefore.UID {
		t.Fatal("pod UID unchanged after image change; expected recreate")
	}
	if podAfter.Spec.Containers[0].Image != "python:3.13-slim" {
		t.Fatalf("pod image = %q after recreate", podAfter.Spec.Containers[0].Image)
	}
	if len(podAfter.Annotations[specHashAnnotation]) == 0 {
		t.Fatal("recreated pod missing spec-hash annotation")
	}

	// PVC retained: same UID as before.
	var pvcAfter corev1.PersistentVolumeClaim
	if err := k8s.Get(ctx, types.NamespacedName{Name: "python-data", Namespace: "default"}, &pvcAfter); err != nil {
		t.Fatal(err)
	}
	if pvcAfter.UID != pvcBefore.UID {
		t.Fatal("data PVC UID changed across pod recreate; PVC must be retained")
	}
}
```

- [ ] **Step 2: Run the test to verify it passes**

This is a test-only task: Task 3's `ensurePod` already deletes+recreates the Pod on
`spec-hash` mismatch and retains the (separately-owned) data PVC, so no controller
change is expected. The re-fetch before `k8s.Update` (in the test above) is required:
the first reconcile writes `.status` (Task 5 `writeStatus`), bumping
`ss.resourceVersion`, so an Update with the stale in-memory object 409-conflicts.
This was verified empirically (envtest): with the re-fetch, the Pod UID changes
(recreate), the image updates to `python:3.13-slim`, and the data PVC UID is unchanged
(retained). Run and confirm PASS.

- [ ] **Step 3: (No controller change expected)**

If the test fails, do NOT change `ensurePod`/`podSpecHash` without first confirming the
failure cause via the test output. The verified-correct path is: re-fetch before Update
(already in the test) → Pod recreated, PVC retained. If the Pod is NOT recreated, the
likely cause is a hash bug in `ensurePod` (it hashes the *existing* pod's values instead
of the *desired* parameters) — confirm `podSpecHash(image, ...)` uses the function
parameters, not `existing`. But this is NOT expected: Task 3's logic was verified working.

- [ ] **Step 4: Run the full controller suite**

```sh
cd operator && go test ./internal/controller/ -run TestServiceSetReconciler -v
```
Expected: all `TestServiceSetReconciler_*` PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/controller/serviceset_controller_test.go
git commit -m "test(controller): image change recreates Pod and retains data PVC"
```

---

### Task 7: Prune children no longer in the spec

**Files:**
- Modify: `operator/internal/controller/serviceset_controller.go` (add `pruneChildren`; call at end of `Reconcile`)
- Test: `operator/internal/controller/serviceset_controller_test.go`

**Interfaces:**
- Produces: `pruneChildren(ctx, ss) error` lists Pods/Services/PVCs in the namespace with `labelServiceset=<ss.Name>` (i.e. owned by this ServiceSet) and deletes any whose `labelEntry` name is not in the current `services`+`runtimes` set. Run last in `Reconcile`.

- [ ] **Step 1: Write the failing test**

```go
func TestServiceSetReconciler_PrunesRemovedEntries(t *testing.T) {
	ss := &sandboxv1alpha1.ServiceSet{Spec: sandboxv1alpha1.ServiceSetSpec{
		EnvironmentName: "env-prune",
		Services: []sandboxv1alpha1.ServiceSpec{
			{Name: "keep", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}},
			{Name: "drop", Image: "alpine:3.21", Command: []string{"sleep", "infinity"}},
		},
	}}
	ss.Name, ss.Namespace = "set-prune", "default"
	mustCreateServiceSet(t, ss)
	r := newServiceSetReconciler(t)
	key := types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}
	reconcileServiceSetOnce(t, r, key)

	// Both pods exist now.
	if err := k8s.Get(ctx, types.NamespacedName{Name: "drop", Namespace: "default"}, &corev1.Pod{}); err != nil {
		t.Fatalf("drop pod should exist: %v", err)
	}

	// Remove "drop" and re-reconcile.
	ss.Spec.Services = ss.Spec.Services[:1] // keep only "keep"
	if err := k8s.Update(ctx, ss); err != nil {
		t.Fatal(err)
	}
	reconcileServiceSetOnce(t, r, key)

	if err := k8s.Get(ctx, types.NamespacedName{Name: "drop", Namespace: "default"}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("drop pod should be pruned, got err=%v", err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "keep", Namespace: "default"}, &corev1.Pod{}); err != nil {
		t.Fatalf("keep pod should remain: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Expected: FAIL — "drop" pod still exists.

- [ ] **Step 3: Implement pruneChildren**

In `serviceset_controller.go`, at the end of `Reconcile` (after `writeStatus`):

```go
	if err := r.pruneChildren(ctx, &ss); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
```

```go
func (r *ServiceSetReconciler) pruneChildren(ctx context.Context, ss *sandboxv1alpha1.ServiceSet) error {
	want := map[string]struct{}{}
	for _, s := range ss.Spec.Services {
		want[s.Name] = struct{}{}
	}
	for _, rt := range ss.Spec.Runtimes {
		want[rt.Name] = struct{}{}
	}
	selector := labels.SelectorFromSet(map[string]string{labelServiceset: ss.Name})

	listOpts := []client.ListOption{client.InNamespace(ss.Namespace), client.MatchingLabelsSelector{Selector: selector}}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, listOpts...); err != nil {
		return err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if _, ok := want[p.Labels[labelEntry]]; ok {
			continue
		}
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	var svcs corev1.ServiceList
	if err := r.List(ctx, &svcs, listOpts...); err != nil {
		return err
	}
	for i := range svcs.Items {
		s := &svcs.Items[i]
		if _, ok := want[s.Labels[labelEntry]]; ok {
			continue
		}
		if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	var pvcs corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcs, listOpts...); err != nil {
		return err
	}
	for i := range pvcs.Items {
		v := &pvcs.Items[i]
		// A data PVC carries its service's entry label, so it prunes alongside
		// the service's Pod/Service.
		if _, ok := want[v.Labels[labelEntry]]; ok {
			continue
		}
		if err := r.Delete(ctx, v); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
```

(Add import: `k8slabels "k8s.io/apimachinery/pkg/labels"`. The `labelEntry` on a data PVC is the service name — set it via `entryLabels` in `ensurePVC`, which already passes `labels` containing `labelEntry` — so the `want[v.Labels[labelEntry]]` check prunes the data PVC alongside its service's Pod/Service.)

- [ ] **Step 4: Run the test to verify it passes**

Expected: PASS. Also re-run the whole suite to ensure no regression:

```sh
cd operator && go test ./internal/controller/ -run TestServiceSetReconciler -v
```

- [ ] **Step 5: Commit**

```sh
git add internal/controller/serviceset_controller.go internal/controller/serviceset_controller_test.go
git commit -m "feat(controller): prune ServiceSet children no longer in the spec"
```

---

### Task 8: Full suite green + helm RBAC drift check + docs note

**Files:**
- Verify: full envtest + `make helm-crds-check` + `make crd-docs-check`
- Modify (if drift): generated artifacts only

**Interfaces:**
- Produces: a green `make test-envtest`, no helm/CRD-doc drift, and a one-paragraph note in `operator/docs/engines.md` describing the `ServiceSet` controller (the design's k8s-native foundation) — no usage docs yet (the agent surface is Plan 2).

- [ ] **Step 1: Run the full envtest suite**

```sh
cd operator && make IN_CONTAINER=1 envtest-assets && make IN_CONTAINER=1 test-envtest 2>&1 | tail -20
```
Expected: PASS, no failures (existing `sandboxenvironment` tests unaffected — the new controller is additive).

- [ ] **Step 2: Helm CRD + RBAC + crd-doc drift checks**

```sh
cd operator && make IN_CONTAINER=1 helm-crds-check && make IN_CONTAINER=1 manifests && git diff --exit-code config/rbac/role.yaml && make IN_CONTAINER=1 crd-docs-check
```
Expected: no drift. If `make IN_CONTAINER=1 manifests` after the RBAC additions in Task 2 produced changes that weren't committed, commit them now.

- [ ] **Step 3: Add a short docs note**

In `operator/docs/engines.md`, add a section near the top:

```markdown
## ServiceSet controller (k8s-native foundation)

A `ServiceSet` (`sandbox.psenna.dev/v1alpha1`, namespaced, short name `sbset`)
declares long-lived dependency **services** and dev-tool **runtimes** for a
`SandboxEnvironment`. The `ServiceSetReconciler` reconciles each entry to native
Pods/Services/PVCs in the ServiceSet's namespace, owns them (garbage-collected
with the ServiceSet), and reports per-entry `Ready` status gated by `dependsOn`.

- **services** → `Service/<name>` (cluster DNS `<name>.<ns>.svc`) + `Pod/<name>`
  + optional `PVC/<name>-data` (RWO, retained by name across Pod recreates).
- **runtimes** → `Pod/<name>` mounting the shared workspace PVC
  (`<environmentName>-workspace`) at `/workspace`; the agent `exec`s into them.

Pod readiness drives the `ServiceSet`'s `Ready` condition. Changing a
pod-affecting field (image/env/command) recreates only that Pod; the data PVC is
retained. Removing an entry prunes its Pod/Service/PVC.

The agent-facing control-API surface (`apply`/`exec`/`compose`) that creates and
drives `ServiceSet`s is documented in Plan 2.
```

- [ ] **Step 4: Commit**

```sh
git add docs/engines.md config/rbac/role.yaml
git commit -m "docs: document the ServiceSet controller (k8s-native foundation)"
```

---

## Self-Review

**Spec coverage (against `docs/superpowers/specs/2026-08-23-k8s-native-execution-model-design.md`):**
- §1 declaration schema (services + runtimes, fields) → Task 1 types. ✓ (Plan 1 covers the CRD shape; the `services.yaml`→CRD parsing is Plan 2.)
- §2 reconcile (ServiceSet CR, identity=name, changed→recreate Pod retain PVC, removed→delete, dependsOn gating, Ready conditions) → Tasks 3, 4, 5, 6, 7. ✓
- §3 exec → Plan 2 (out of scope here; noted in Task 8 docs).
- §4 security/isolation (no hostPath/privileged, NetworkPolicy inherited) → Global Constraints + the controller creates only ordinary Pods/Services/PVCs with no privileged/hostPath fields. ✓
- §5 storage (RWO workspace shared on single node; per-service RWO data PVC retained) → Tasks 3, 4, 6 + Global Constraints. ✓ (RWX-on-RWO+affinity multi-node path is deferred, per the spec's "adapt" note — Plan 1 ships the single-node-correct RWO behavior.)
- §6 test surface — the envtest coverage (Tasks 3–7) is the unit slice; e2e is Plan 2/3. ✓
- §7 scope cuts (no ephemeral run, no image build) — Plan 1 adds no such surface. ✓
- §8 open decisions — `ServiceSet` as separate CR (Task 1) ✓; RWO+affinity vs RWX → Plan 1 uses RWO single-node (documented), deferring the affinity/RWX machinery. ✓

**Placeholder scan:** No TBD/TODO. The one dead-code block in Task 7 Step 3 is explicitly called out for cleanup before commit. All code steps contain real code. All referenced types (`ServiceSet`, `ServiceSpec`, `RuntimeSpec`, `HealthcheckSpec`, `ServiceStorageSpec`, `EntryStatus`) are defined in Task 1. All helpers (`ensurePod`, `podSpecHash`, `isPodReady`, `computeReady`, `pruneChildren`, `entryLabels`, `ownerRef`, `readinessProbe`, `toEnvVars`) are defined within these tasks.

**Type consistency:** `ServiceSetReconciler`, `Reconcile`, `SetupWithManager`, `reconcileServiceSetOnce`, `entryLabels`, `ensurePod`, `podSpecHash`, `isPodReady`, `computeReady`, `writeStatus`, `pruneChildren`, `ownerRef`, `readinessProbe` — names match across tasks. Label constants (`labelServiceset`, `labelEntry`, `labelKind`, `specHashAnnotation`) defined once (Task 3) and reused. `readyMap`/`computeReady` closure type is defined in Task 5 and used there.

**Scope check:** One coherent subsystem (CRD + controller), envtest-testable end-to-end (create a ServiceSet, get children, simulate readiness, change spec, prune). Plans 2 and 3 are explicitly deferred and named. This plan is focused enough for a single implementation pass.