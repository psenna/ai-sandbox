package sandboxctl

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// EngineTeardown stops and removes every workload container the sandbox's
// container engine is running, BEFORE the workspace is archived. This is a
// snapshot-INTEGRITY requirement, not tidiness: workload containers
// bind-mount /workspace, so a tar taken while one is still writing is torn.
//
// The seam lives here, not on internal/render's Engine interface, for two
// reasons: render.Engine.Contribute is contractually PURE and render-time
// only (see internal/render/engine.go), and teardown must run in THIS
// process, inside the pod, against a pod-local engine socket the operator
// cannot reach. The real rootless-podman implementation lands with #24.
type EngineTeardown interface {
	Teardown(ctx context.Context) (TeardownReport, error)
}

// TeardownReport is what actually happened, recorded verbatim into the
// resume marker (marker.go) so the agent is never told a comforting lie.
type TeardownReport struct {
	Engine     string   // "none" | "rootless-podman" | "k8s-native"
	Containers []string // podman: stopped+removed, sorted, deterministic
	Pods       []string // k8s-native: ServiceSet pods present at freeze, sorted
	Note       string   // human-readable, surfaced in RESUME.md
}

// noopEngineTeardown is a TRUE no-op, mirroring internal/render's
// noneEngine: engine.type=none contributes zero containers, so there is
// genuinely nothing to tear down.
type noopEngineTeardown struct{}

func (noopEngineTeardown) Teardown(context.Context) (TeardownReport, error) {
	return TeardownReport{
		Engine: "none",
		Note:   "engine.type=none: no workload containers to tear down",
	}, nil
}

// notImplementedEngineTeardown fails closed, naming #24 -- mirroring
// internal/render/engine.go's notImplementedEngine. Unreachable today:
// RenderPod refuses to render a rootless-podman pod at all, so no such pod
// (and no such sidecar) can exist yet.
type notImplementedEngineTeardown struct {
	engine string
	issue  int
}

func (e notImplementedEngineTeardown) Teardown(context.Context) (TeardownReport, error) {
	return TeardownReport{}, fmt.Errorf("sandboxctl: engine %q teardown is not implemented yet (see issue #%d)", e.engine, e.issue)
}

// NewEngineTeardown selects the EngineTeardown for engineType. The k8s-native
// teardown reads the env's ServiceSet (named envName in namespace) to enumerate
// its pods, so it needs the in-cluster client + the env identity; the other
// engines ignore them.
func NewEngineTeardown(engineType string, c client.Client, namespace, envName string) EngineTeardown {
	switch engineType {
	case "", "none":
		return noopEngineTeardown{}
	case "rootless-podman":
		return notImplementedEngineTeardown{engine: "rootless-podman", issue: 24}
	case "k8s-native":
		return k8sNativeEngineTeardown{c: c, namespace: namespace, envName: envName}
	default:
		return notImplementedEngineTeardown{engine: engineType, issue: 24}
	}
}

// k8sNativeEngineTeardown is a LIST-ONLY teardown: it reads the env's ServiceSet
// and reports its entries' pod names (pod name == entry name) in TeardownReport.Pods.
// It does NOT delete the ServiceSet or its pods. Rationale: service data
// PVCs are owned by the ServiceSet (serviceset_controller.go's ownerRef), so
// deleting the ServiceSet would GC the data PVCs (data loss across freeze/wake);
// deleting only pods triggers a ServiceSetReconciler recreate storm. RWO co-mount
// lets the snapshot Job archive the workspace PVC without evicting pods, and the
// agent quiesces (sandbox-wait) before freeze. A follow-up tracks a suspend-aware
// teardown that stops writers without storm/data-loss.
type k8sNativeEngineTeardown struct {
	c         client.Client
	namespace string
	envName   string
}

func (e k8sNativeEngineTeardown) Teardown(ctx context.Context) (TeardownReport, error) {
	var ss sandboxv1alpha1.ServiceSet
	if err := e.c.Get(ctx, client.ObjectKey{Name: e.envName, Namespace: e.namespace}, &ss); err != nil {
		if apierrors.IsNotFound(err) {
			return TeardownReport{
				Engine: "k8s-native",
				Note:   "k8s-native: no ServiceSet applied (no pods to tear down)",
			}, nil
		}
		return TeardownReport{}, fmt.Errorf("reading ServiceSet %s/%s: %w", e.namespace, e.envName, err)
	}
	names := make([]string, 0, len(ss.Status.Entries))
	for _, entry := range ss.Status.Entries {
		names = append(names, entry.Name)
	}
	sort.Strings(names)
	return TeardownReport{
		Engine: "k8s-native",
		Pods:   names,
		Note:   fmt.Sprintf("k8s-native: %d pod(s) present at freeze (list-only; reconciler-managed; data PVCs retained)", len(names)),
	}, nil
}
