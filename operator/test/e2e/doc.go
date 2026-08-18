// Package e2e is the operator's kind-based end-to-end test suite
// (issue #22): a Ginkgo/Gomega suite that stands up a real kind cluster,
// deploys the real operator image alongside MinIO and two dependency-free
// platform-service doubles (a fake git-proxy broker and a stub model
// endpoint), and drives real SandboxClass/SandboxEnvironment objects
// through their full lifecycle against the real Kubernetes API -- the one
// layer envtest (internal/apitest, internal/controller's suite_test.go)
// cannot cover: real kubelet pod scheduling/execution, a real CNI's
// NetworkPolicy enforcement, and real image pulls.
//
// # Why a separate Go module
//
// This directory is its own Go module (see go.mod), never referenced from
// operator/go.mod. Ginkgo's reporters transitively import
// golang.org/x/tools/go/packages -> golang.org/x/mod/semver, and
// DependaProxy's CVE gate denies golang.org/x/mod at every released
// version. Keeping ginkgo/gomega confined to this module means that
// unresolvable requirement never touches the operator's own build, test,
// vet, lint, vuln or Docker build paths -- only `make e2e` (and its
// sub-targets) ever resolves this module, through the same file-cache
// GOPROXY chain operator/hack/tools already uses (see operator/README.md
// and TOOLS_GOPROXY in operator/Makefile).
//
// # How to run
//
// From operator/:
//
//	make e2e            # up -> run -> down, tears down even on failure
//	make e2e-up          # stand up the kind cluster + platform, leave it running
//	make e2e-run          # run the Ginkgo suite against an already-up cluster
//	make e2e-down         # tear down
//	make e2e-collect      # dump cluster diagnostics to test/e2e/_artifacts
//	E2E_KEEP=1 make e2e   # skip the final teardown, for debugging a failure
//
// # What this suite does NOT test
//
// See the issue's non-goals: no NetworkPolicy rendering (the operator
// renders none today), no wait-probe evaluation (#30), no archive logic
// (#32), no rootless-podman engine (#24). Freeze (snapshot, upload,
// teardown, slot release) IS now tested -- see freeze_test.go (#28). Wake/
// restore is NOT tested: no code path restores a snapshot yet (#29); the
// "sequence numbers increase monotonically across repeated freezes" spec in
// freeze_test.go simulates only the status.waitFor=nil half of a wake
// (exercising the scheduler/pod-recreation machinery against the still-
// intact workspace PVC), never an actual restore. The fixtures for several
// of these (MinIO, the platform doubles) are stood up and self-tested here
// so that the issues that DO implement that logic can build on a working
// harness without also having to build the harness.
package e2e
