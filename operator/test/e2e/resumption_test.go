package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// The context-resumption spec (#29), the issue's Layer-1 gate turned into a
// permanent regression test. The agent home is an emptyDir that dies with
// the pod, so the ONLY way a file written there in run 1 can be read by run
// 2 is freeze -> S3 (as agent-home.tar.zst) -> wake into a fresh pod.
//
// Every assertion of the run-2 half runs INSIDE the woken pod, via the fake
// claude's SCRIPT: directives: require-home-file proves the transcript bytes
// came back over the wire and are readable by the agent process itself;
// require-tree-sha256 proves the workspace tree is byte-for-byte identical
// (same files, modes, sizes, hashes, symlink targets) to what run 1 saw;
// require-file /workspace/.sandbox/last-wake.json proves the restore init
// container actually ran before the agent was started. The status-side
// assertions (roots[agent-home].source == Cold with bytesDownloaded > 0)
// corroborate the same thing from the operator's own records.
//
// The CEL `task is immutable` rule (api/v1alpha1) forbids patching spec.task
// between the freeze and the wake, and the real product re-runs the same task
// on a wake (the agent's unfreeze skill resumes from last-wake.json) -- so a
// single task.prompt self-branches on the wake marker
// (.sandbox/last-wake.json, written by the restore init container ONLY on a
// wake -- see render/pod.go's `in.Restore != nil` gate), using the fake
// agent's unless-file/if-file directives:
//   - run 1 (no marker, fresh pod with no restore init container): write the
//     transcript into the agent home, write the workspace file, record the
//     workspace tree digest into the agent home (a DIFFERENT tree so the
//     digest file can never contaminate what it describes), then declare a
//     wait so the sidecar can freeze the pod. The digest is written before
//     the wait so the snapshot archives it (as agent-home.tar.zst) alongside
//     the workspace.
//   - the woken run (marker present, restore init container ran): assert the
//     restoration -- the transcript came back over the wire and is readable
//     by the agent (require-home-file), the workspace tree is byte-for-byte
//     what run 1 saw (require-tree-sha256 against the digest file that
//     traveled through agent-home.tar.zst), and the restore init container
//     actually ran (require-file on the marker) -- and only then report
//     success. Reaching Done with exit 0 means every require-* passed, so the
//     spec never has to parse pod logs to prove the restore worked.
var _ = Describe("resumption", func() {
	var (
		ctx context.Context
		ns  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		ns = h.NewNamespace(ctx, "e2e-resume")
	})

	It("carries the agent home and workspace across a freeze->wake into a fresh pod", func() {
		// A long warmCacheTTL keeps the warm-cache GC (#29) out of this
		// spec's way entirely -- resumption is about the S3 round-trip, not
		// about cache reclamation.
		class := h.CreateClass(ctx, WithWarmCacheTTL("24h"))

		token := randHex(8) // unique per run: a "transcript" whose bytes must survive
		// One immutable prompt self-branches on the wake marker: the
		// unless-file half is run 1 (no marker), the if-file half is the
		// woken run (marker present). See the header comment for why a single
		// prompt is correct (#29: spec.task is CEL-immutable).
		script := fmt.Sprintf(`SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:write-home projects/-workspace/session.jsonl %s
SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:write hello.txt world
SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:tree-sha256 /workspace /home/node/.claude-sandbox/tree-sha256.txt
SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:sandbox-wait {"type":"NotBefore","reason":"e2e resumption","params":{"duration":"1h"}}
SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:sleep 900
SCRIPT:if-file /workspace/.sandbox/last-wake.json SCRIPT:require-home-file projects/-workspace/session.jsonl %s
SCRIPT:if-file /workspace/.sandbox/last-wake.json SCRIPT:append-home projects/-workspace/session.jsonl continued
SCRIPT:if-file /workspace/.sandbox/last-wake.json SCRIPT:require-tree-sha256 /workspace /home/node/.claude-sandbox/tree-sha256.txt
SCRIPT:if-file /workspace/.sandbox/last-wake.json SCRIPT:require-file /workspace/.sandbox/last-wake.json
SCRIPT:if-file /workspace/.sandbox/last-wake.json SCRIPT:sandbox-done success resumed`, token, token)

		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(script))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		// Freeze: Waiting is reached only after a real, successful snapshot
		// (#28), so once we are here both archives exist in S3.
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		got := h.GetEnv(ctx, key)
		snap := got.Status.Snapshot
		Expect(snap).NotTo(BeNil())

		// Step 2 of the plan: verify via S3 that the manifest lists BOTH the
		// workspace and the agent-home archive, and that every file it lists
		// verifies checksum-for-checksum (the same manifest walk
		// freeze_test.go performs).
		layout := envLayout(got)
		at := snap.TakenAt.Time
		manifestBytes := h.WaitForS3Object(ctx, h.Cfg.SnapshotBucket, layout.Manifest(int(snap.Seq), at), h.Cfg.PodTimeout)
		m, err := storage.ParseManifest(bytes.NewReader(manifestBytes))
		Expect(err).NotTo(HaveOccurred())
		_, hasWorkspace := m.File(storage.FileWorkspace)
		_, hasAgentHome := m.File(storage.FileAgentHome)
		Expect(hasWorkspace).To(BeTrue(), "manifest must list workspace.tar.zst")
		Expect(hasAgentHome).To(BeTrue(), "manifest must list agent-home.tar.zst (the resumption proof)")
		for _, f := range m.Files {
			body, err := h.S3Get(ctx, h.Cfg.SnapshotBucket, layout.SnapshotFile(int(snap.Seq), at, f.Name))
			Expect(err).NotTo(HaveOccurred(), "fetching manifest-listed file %s", f.Name)
			sum := sha256.Sum256(body)
			Expect(hex.EncodeToString(sum[:])).To(Equal(f.SHA256), "checksum mismatch for %s", f.Name)
		}

		// Wake: the environment keeps its original (single, self-branching)
		// prompt; the woken run's if-file half asserts the restoration inside
		// the fresh pod. No prompt patch (#29: spec.task is CEL-immutable) --
		// clearing the wait is enough.
		h.ClearWaitFor(ctx, key)

		// The run-2 script's sandbox-done + exit 0 is what carries the
		// environment to Done; reaching it means every require-* directive
		// above passed inside the woken pod.
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.ExpectAgentExitCode(ctx, key, 0)

		final := h.GetEnv(ctx, key)
		Expect(final.Status.WakeCount).To(Equal(int32(1)), "exactly one wake")

		attempt := final.Status.RestoreAttempt
		Expect(attempt).NotTo(BeNil(), "restoreAttempt must be recorded on wake")
		Expect(attempt.Phase).To(Equal(sandboxv1alpha1.RestoreAttemptSucceeded))

		// The transcript demonstrably came back over the wire: the agent
		// home is an emptyDir, so any restored bytes necessarily crossed S3
		// into a fresh pod with a fresh container identity.
		home := rootByName(attempt.Roots, "agent-home")
		Expect(home).NotTo(BeNil(), "restoreAttempt.roots must record agent-home")
		Expect(home.Source).To(Equal("Cold"), "agent home is always restored cold (emptyDir dies with the pod)")
		Expect(home.BytesDownloaded).To(BeNumerically(">", 0))

		Expect(rootByName(attempt.Roots, "workspace")).NotTo(BeNil(), "restoreAttempt.roots must record workspace")
	})
})
