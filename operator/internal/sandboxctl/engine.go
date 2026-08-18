package sandboxctl

import (
	"context"
	"fmt"
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

// NewEngineTeardown selects the EngineTeardown for engineType, normalizing
// "" to "none" (the only implemented engine).
func NewEngineTeardown(engineType string) EngineTeardown {
	switch engineType {
	case "", "none":
		return noopEngineTeardown{}
	case "rootless-podman":
		return notImplementedEngineTeardown{engine: "rootless-podman", issue: 24}
	default:
		return notImplementedEngineTeardown{engine: engineType, issue: 24}
	}
}
