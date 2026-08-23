package sandboxctl

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
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
	Engine     string   // "none" | "rootless-podman"
	Containers []string // stopped+removed, sorted, deterministic
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

// notImplementedEngineTeardown fails closed, naming the issue that ships a
// real implementation -- mirroring internal/render/engine.go's
// notImplementedEngine. Reachable only for an engine type this operator does
// not know at all (or a future engine, #25); both real v1 engine types
// (none, rootless-podman) now have real implementations.
type notImplementedEngineTeardown struct {
	engine string
	issue  int
}

func (e notImplementedEngineTeardown) Teardown(context.Context) (TeardownReport, error) {
	return TeardownReport{}, fmt.Errorf("sandboxctl: engine %q teardown is not implemented yet (see issue #%d)", e.engine, e.issue)
}

// failedEngineTeardown fails closed on a construction error (e.g. an
// unparseable --engine-endpoint) -- Teardown returns the error the
// constructor hit, rather than a nil EngineTeardown that would panic the
// caller or a noop that would silently skip a real teardown.
type failedEngineTeardown struct {
	engine string
	err    error
}

func (e failedEngineTeardown) Teardown(context.Context) (TeardownReport, error) {
	return TeardownReport{}, fmt.Errorf("sandboxctl: engine %q teardown unavailable: %w", e.engine, e.err)
}

// NewEngineTeardown selects the EngineTeardown for engineType, normalizing
// "" to "none". endpoint is the pod-loopback engine API endpoint (#24:
// render.PodmanDockerHost, projected via sidecarSnapshotArgs'
// --engine-endpoint flag); ignored for engine types that need none.
func NewEngineTeardown(engineType, endpoint string, log logr.Logger) EngineTeardown {
	switch engineType {
	case "", "none":
		return noopEngineTeardown{}
	case "rootless-podman":
		t, err := newPodmanTeardown(endpoint, log)
		if err != nil {
			return failedEngineTeardown{engine: engineType, err: err}
		}
		return t
	default:
		return notImplementedEngineTeardown{engine: engineType, issue: 25}
	}
}
