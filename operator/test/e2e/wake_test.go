package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/render"
	"github.com/psenna/ai-sandbox/operator/internal/sandboxctl"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// rootByName returns the RestoredRootStatus for the named root (workspace or
// agent-home), or nil. restoreAttempt.roots is a listMap keyed by name, so
// lookup is by name, not by index.
func rootByName(roots []sandboxv1alpha1.RestoredRootStatus, name string) *sandboxv1alpha1.RestoredRootStatus {
	for i := range roots {
		if roots[i].Name == name {
			return &roots[i]
		}
	}
	return nil
}

// wakeScript is the single, immutable task.prompt the warm/cold wake specs
// share. The CEL `task is immutable` rule (api/v1alpha1) forbids patching
// spec.task between the freeze and the wake, and the real product re-runs the
// same task on a wake (the agent's unfreeze skill resumes from last-wake.json)
// -- so the spec cannot swap a run-1 script for a run-2 script the way an
// earlier draft did. Instead one prompt self-branches on the wake marker
// (.sandbox/last-wake.json, written by the restore init container ONLY on a
// wake -- see render/pod.go's `in.Restore != nil` gate), using the fake agent's
// unless-file/if-file directives:
//
//   - run 1 (no marker, fresh pod with no restore init container): write the
//     observable workspace tree, record its digest into the agent home (a
//     DIFFERENT tree so the digest file can never contaminate what it
//     describes), then declare a wait so the sidecar can freeze the pod. The
//     digest is written before the wait so the snapshot archives it alongside
//     the workspace.
//   - the woken run (marker present, restore init container ran): assert the
//     restoration -- the workspace tree is byte-for-byte what run 1 saw
//     (require-tree-sha256) and the restore init container actually ran
//     (require-file on the marker) -- and only then report success. The
//     environment reaching Done with exit code 0 means every require-* passed,
//     so the specs never have to parse pod logs to prove the restore worked.
func wakeScript() string {
	return `SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:write hello.txt world
SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:tree-sha256 /workspace /home/node/.claude-sandbox/tree-sha256.txt
SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:sandbox-wait {"type":"NotBefore","reason":"e2e wake","params":{"duration":"1h"}}
SCRIPT:unless-file /workspace/.sandbox/last-wake.json SCRIPT:sleep 900
SCRIPT:if-file /workspace/.sandbox/last-wake.json SCRIPT:require-tree-sha256 /workspace /home/node/.claude-sandbox/tree-sha256.txt
SCRIPT:if-file /workspace/.sandbox/last-wake.json SCRIPT:require-file /workspace/.sandbox/last-wake.json
SCRIPT:if-file /workspace/.sandbox/last-wake.json SCRIPT:sandbox-done success resumed`
}

// expectNoLeakedResources asserts the workspace-PVC reclamation bookkeeping
// stayed clean: exactly one PVC carries env's owner reference (the workspace
// PVC itself, under its canonical name) and no recovery snapshot Job exists.
// Called at every cycle boundary of the multi-cycle spec, not just at the
// end, so a leak is caught the moment it appears.
func expectNoLeakedResources(ctx context.Context, ns string, env *sandboxv1alpha1.SandboxEnvironment) {
	var list corev1.PersistentVolumeClaimList
	ExpectWithOffset(1, h.Client.List(ctx, &list, client.InNamespace(ns))).To(Succeed(), "listing PVCs in %s", ns)
	var owned []corev1.PersistentVolumeClaim
	for _, p := range list.Items {
		if metav1.IsControlledBy(&p, env) {
			owned = append(owned, p)
		}
	}
	ExpectWithOffset(1, owned).To(HaveLen(1), "exactly one PVC may carry %s's owner reference", env.Name)
	ExpectWithOffset(1, owned[0].Name).To(Equal(render.ChildNames(env.Name).PVC),
		"the owned PVC must be the canonical workspace PVC")

	jobKey := client.ObjectKey{Namespace: ns, Name: render.ChildNames(env.Name).SnapshotJob}
	var job batchv1.Job
	err := h.Client.Get(ctx, jobKey, &job)
	ExpectWithOffset(1, apierrors.IsNotFound(err)).To(BeTrue(),
		"no recovery Job may exist after a clean freeze (it is only created when a pod is lost mid-freeze)")
}

// wakeIntoCorruptedWorkspace freezes run 1 cleanly, then makes the wake read
// a corrupted workspace.tar.zst (mode: "corrupt-read" flips a byte mid-stream,
// "truncate-read" closes the body early) and waits until the environment has
// terminally failed the restore.
func wakeIntoCorruptedWorkspace(ctx context.Context, key client.ObjectKey, ns string, mode string) {
	// The freeze's final step (snapshot.go step 10) writes warm-cache.json,
	// so a wake with the PVC intact would restore the workspace WARM and the
	// corrupt archive would never be downloaded -- the corruption would be
	// invisible and the spec would pass for the wrong reason. Delete the PVC
	// while Waiting (the same reclamation the TTL GC performs, and safe for
	// the same reason: resources.go skips the PVC apply while Waiting) to
	// force the cold path deterministically.
	pvcs := h.ListEnvPVCs(ctx, ns, key.Name)
	ExpectWithOffset(1, pvcs).To(HaveLen(1), "workspace PVC must exist before it can be deleted")
	ExpectWithOffset(1, h.Client.Delete(ctx, &pvcs[0])).To(Succeed())
	pvcKey := client.ObjectKey{Namespace: ns, Name: render.ChildNames(key.Name).PVC}
	h.WaitForPVCGone(ctx, pvcKey, h.Cfg.PodTimeout)

	// Corrupt ONLY workspace.tar.zst (pathSuffix filter); manifest.json must
	// stay intact so the failure is the CHECKSUM path, not a manifest parse
	// error. Cleared unconditionally so a failure here can't wedge the shared
	// proxy for every later spec.
	h.S3ProxySetFaultOn(ctx, mode, 200, 4096, "workspace.tar.zst")
	DeferCleanup(func(ctx context.Context) { h.S3ProxyClearFault(ctx) })

	// The wake itself: the env keeps its original run-1 script; the restore
	// init container fails (the corrupted archive) before the agent ever
	// starts, so any woken-run assertions would never execute anyway -- no
	// prompt patch is needed (#29: spec.task is CEL-immutable either way).
	h.ClearWaitFor(ctx, key)

	h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseFailed, h.Cfg.PhaseTimeout)
	h.WaitForCondition(ctx, key, "PodReady", metav1.ConditionFalse, lifecycle.ReasonRestoreVerificationFailed, h.Cfg.PhaseTimeout)
	h.WaitForCondition(ctx, key, "Ready", metav1.ConditionFalse, lifecycle.ReasonRestoreVerificationFailed, h.Cfg.PhaseTimeout)

	attempt := h.WaitForRestoreAttempt(ctx, key, sandboxv1alpha1.RestoreAttemptFailed, h.Cfg.PodTimeout)
	ExpectWithOffset(1, attempt.Reason).To(Equal(sandboxctl.RestoreReasonChecksumMismatch),
		"the restore must fail on the checksum, not on a manifest parse")

	// The agent container never started: the restore init container is a
	// plain, non-restartable init container ordered LAST, so its failure
	// failed the pod before the agent could ever be created -- the kubelet's
	// own ordering guarantee, not a timing inference.
	// The env reaches Failed as soon as the controller's cache reflects the
	// restore init container's Terminated(non-zero) (podstatus.go
	// restoreFailure), which can run ahead of this client's own cache
	// reflecting the pod's phase flip to Failed. Poll the phase rather than
	// reading once, so the spec does not flake on a Pending snapshot the
	// controller has already moved past. The failed pod is kept (not
	// deleted) for inspection, so it persists across the poll.
	EventuallyWithOffset(1, func() corev1.PodPhase {
		return h.GetAgentPod(ctx, key).Status.Phase
	}, h.Cfg.PodTimeout, h.Cfg.Poll).Should(Equal(corev1.PodFailed),
		"the failed restore must fail the pod (it is kept for inspection, not deleted)")

	pod := h.GetAgentPod(ctx, key)
	var agent corev1.ContainerStatus
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == render.AgentContainerName {
			agent = cs
			break
		}
	}
	ExpectWithOffset(1, agent.State.Terminated).To(BeNil(), "the agent must never have run")
	ExpectWithOffset(1, agent.State.Running).To(BeNil(), "the agent must never have run")
	ExpectWithOffset(1, agent.State.Waiting).NotTo(BeNil())
	ExpectWithOffset(1, agent.State.Waiting.Reason).To(Equal("PodInitializing"),
		"the agent stays Waiting/PodInitializing behind the failed restore init container")
}

var _ = Describe("wake", func() {
	var (
		ctx context.Context
		ns  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		ns = h.NewNamespace(ctx, "e2e-wake")
	})

	// The warm path is the #29 optimization whose correctness the whole warm
	// cache exists to serve. This spec is the mirror image of the TTL-GC
	// spec below: the freeze wrote warm-cache.json (snapshot.go step 10), the
	// restore container cross-checks it against the manifest and the teardown
	// marker (warmmarker.go's ValidateWarmMarker) and skips the download.
	It("restores the workspace warm from the retained PVC when the warm marker validates", func() {
		// A long warmCacheTTL keeps the TTL GC (#29) out of this spec's way
		// entirely -- this is about the warm path, not about reclamation.
		class := h.CreateClass(ctx, WithWarmCacheTTL("24h"))
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(wakeScript()))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)
		got := h.GetEnv(ctx, key)
		snap := got.Status.Snapshot
		Expect(snap).NotTo(BeNil())
		wantSnapshotID := storage.SnapshotID(int(snap.Seq), snap.TakenAt.Time)

		// The wake runs the SAME self-branching task the freeze did; no prompt
		// patch (#29: spec.task is CEL-immutable). Clearing the wait is enough.
		h.ClearWaitFor(ctx, key)

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.ExpectAgentExitCode(ctx, key, 0)

		final := h.GetEnv(ctx, key)
		Expect(final.Status.WakeCount).To(Equal(int32(1)), "exactly one wake")

		attempt := h.WaitForRestoreAttempt(ctx, key, sandboxv1alpha1.RestoreAttemptSucceeded, h.Cfg.PodTimeout)
		Expect(attempt.SnapshotID).To(Equal(wantSnapshotID), "restoreAttempt must record the exact snapshot restored")

		ws := rootByName(attempt.Roots, "workspace")
		Expect(ws).NotTo(BeNil(), "restoreAttempt.roots must record workspace")
		Expect(ws.Source).To(Equal("Warm"), "the freeze wrote warm-cache.json, so this wake must not download")
		Expect(ws.BytesDownloaded).To(BeZero(), "the acceptance criterion's measurable download-skip, from a status field, not from timing")

		home := rootByName(attempt.Roots, "agent-home")
		Expect(home).NotTo(BeNil(), "restoreAttempt.roots must record agent-home")
		Expect(home.Source).To(Equal("Cold"), "the agent home is an emptyDir: no warm path can exist")
		Expect(home.BytesDownloaded).To(BeNumerically(">", 0))
	})

	// The TTL GC (warmcachegc.go) reclaims the workspace PVC of a Waiting
	// environment once its snapshot is older than the class's warmCacheTTL.
	// This spec is the acceptance criterion "after the warm PVC is GC'd, the
	// environment still wakes correctly from S3": the wake must fall back to
	// a full cold restore, with the miss reason recorded and the tree still
	// byte-for-byte identical.
	It("cold-restores from S3 after the TTL GC reclaims the warm PVC", func() {
		class := h.CreateClass(ctx, WithWarmCacheTTL("10s"))
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(wakeScript()))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		// The GC pass runs every 5s (the e2e overlay's
		// --warm-cache-gc-interval); with a 10s TTL the workspace PVC is
		// reclaimed within a few passes. kind runs kube-controller-manager,
		// so the PVC actually disappears rather than lingering with a
		// deletionTimestamp.
		pvcKey := client.ObjectKey{Namespace: ns, Name: render.ChildNames(env.Name).PVC}
		h.WaitForPVCGone(ctx, pvcKey, h.Cfg.PhaseTimeout)
		Expect(h.ListEnvPVCs(ctx, ns, env.Name)).To(BeEmpty(), "the reclaimed PVC must be gone, not lingering")

		// The wake runs the SAME self-branching task the freeze did; no prompt
		// patch (#29: spec.task is CEL-immutable). Clearing the wait is enough.
		h.ClearWaitFor(ctx, key)

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.ExpectAgentExitCode(ctx, key, 0)

		attempt := h.WaitForRestoreAttempt(ctx, key, sandboxv1alpha1.RestoreAttemptSucceeded, h.Cfg.PodTimeout)
		ws := rootByName(attempt.Roots, "workspace")
		Expect(ws).NotTo(BeNil())
		Expect(ws.Source).To(Equal("Cold"), "the GC'd PVC leaves no warm cache to restore from")
		Expect(ws.WarmMissReason).To(Equal(sandboxctl.WarmMissNoMarker),
			"the miss reason must record that there was no warm-cache.json to validate")
		Expect(ws.BytesDownloaded).To(BeNumerically(">", 0))

		// Done with exit 0 means require-tree-sha256 passed inside the woken
		// pod: the tree restored from S3 matches run 1 byte-for-byte.
	})

	// The GC's eligibility is phase-gated (warmcachegc.go): only an exactly-
	// Waiting environment with a complete, verified snapshot is reclaimed.
	// A Running environment has no snapshot yet -- its PVC is the only copy
	// of the agent's context until one lands -- so it must survive every GC
	// pass, even with a TTL short enough that a phase-eligibility bug would
	// fire within the window.
	It("never GCs the workspace PVC of a Running environment", func() {
		class := h.CreateClass(ctx, WithWarmCacheTTL("10s"))
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:write hello.txt world",
			"SCRIPT:sleep 900", // never declares a wait: stays Running for the whole window
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseRunning, h.Cfg.PhaseTimeout)

		// 20s at a 5s GC interval is 4+ passes; the TTL has long expired by
		// the window's end, so only the phase gate can be keeping the PVC.
		Consistently(func() bool {
			return len(h.ListEnvPVCs(ctx, ns, env.Name)) == 1
		}, 20*time.Second, time.Second).Should(BeTrue(),
			"a Running environment's workspace PVC must never be reclaimed")
	})

	// The other half of GC safety: an environment whose freeze FAILED is
	// stuck in Freezing with its pod (and workspace) still alive, because
	// nothing was ever uploaded -- deleting the PVC would destroy the only
	// copy of the agent's context. The GC must leave it alone indefinitely,
	// and the SnapshotAttempt==Failed guard would block it even if the phase
	// gate were wrong.
	It("keeps the workspace PVC of an environment whose freeze failed", Serial, func() {
		// Serial: the fail-write fault is a global property of the shared
		// s3proxy double, not scoped per-namespace -- see freeze_test.go's
		// backend-outage spec for why every fault-programming spec is Serial.
		h.S3ProxySetFault(ctx, "fail-write", 503, 4096)
		DeferCleanup(func(ctx context.Context) { h.S3ProxyClearFault(ctx) })

		class := h.CreateClass(ctx, WithS3Endpoint(h.S3ProxyInClusterEndpoint()), WithWarmCacheTTL("10s"))
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:write hello.txt world",
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"failed freeze","params":{"duration":"1h"}}`,
			"SCRIPT:sleep 900",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForCondition(ctx, key, "Frozen", metav1.ConditionFalse, "SnapshotFailed", s3FaultTimeout)
		h.ExpectStablePhase(ctx, key, sandboxv1alpha1.PhaseFreezing, 20*time.Second)

		Consistently(func() bool {
			return len(h.ListEnvPVCs(ctx, ns, env.Name)) == 1
		}, 20*time.Second, time.Second).Should(BeTrue(),
			"a failed freeze must keep its PVC -- it is the only copy of the agent's context")
	})

	// The restore init container verifies every archived root against the
	// manifest's recorded checksum as it streams (storage.RestoreFrom).
	// A corrupted archive must fail the wake loudly -- the environment goes
	// Failed with a checksum-mismatch restore attempt, the agent never
	// starts, and the pod is kept for inspection -- never silently restore a
	// wrong tree.
	It("fails the wake when the workspace archive is corrupted mid-stream", Serial, func() {
		class := h.CreateClass(ctx, WithS3Endpoint(h.S3ProxyInClusterEndpoint()))
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:write-tree filler.bin 262144", // 256KiB of urandom: incompressible, so workspace.tar.zst >> afterBytes
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"corruption","params":{"duration":"1h"}}`,
			"SCRIPT:sleep 900",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		wakeIntoCorruptedWorkspace(ctx, key, ns, "corrupt-read")
	})

	// The truncated-tail variant: the archive's byte count comes up short of
	// the manifest's recorded size -- the case RestoreFrom's drain-before-
	// Verify ordering comment exists for. Same terminal outcome.
	It("fails the wake when the workspace archive is truncated", Serial, func() {
		class := h.CreateClass(ctx, WithS3Endpoint(h.S3ProxyInClusterEndpoint()))
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:write-tree filler.bin 262144",
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"truncation","params":{"duration":"1h"}}`,
			"SCRIPT:sleep 900",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		wakeIntoCorruptedWorkspace(ctx, key, ns, "truncate-read")
	})

	// Three wake cycles on one environment must leave exactly one PVC (the
	// workspace PVC -- every wake restores warm onto it, it is never
	// recreated) and no recovery snapshot Job behind. Wakes are triggered by
	// clearing status.waitFor with the admin client (#30 will do this
	// automatically; these specs deliberately do it by hand); after each
	// wake the same run script declares the same wait again, so the
	// environment re-freezes and the cycle repeats.
	It("never leaks PVCs or recovery Jobs across repeated freezes and wakes", func() {
		class := h.CreateClass(ctx, WithWarmCacheTTL("24h"))
		run := `SCRIPT:write hello.txt world
SCRIPT:sandbox-wait {"type":"NotBefore","reason":"e2e no-leak cycles","params":{"duration":"1h"}}
SCRIPT:sleep 900`
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(run))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		// Freeze #1 (before any wake), then three wake cycles -- each wake is
		// followed by a re-freeze because the same script declares the same
		// wait again: WakeCount 3, FreezeCount 4.
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)
		expectNoLeakedResources(ctx, ns, env)

		for cycle := 1; cycle <= 3; cycle++ {
			h.ClearWaitFor(ctx, key) // wake
			// Wait for WakeCount to reach `cycle` BEFORE waiting for the
			// re-freeze. WakeCount increments (monotonically, only on the
			// Restoring->Running transition) and never resets, so it cannot
			// match a stale pre-wake value -- which is exactly what a bare
			// WaitForPhase(Waiting) races: immediately after ClearWaitFor the
			// env is still Waiting (the controller has not yet reconciled it
			// toward Ready), so WaitForPhase(Waiting) returns the PRE-wake
			// Waiting with WakeCount 0 and the spec fails for the wrong
			// reason. WaitForWakeCount forces a real wake to complete first;
			// the subsequent WaitForPhase(Waiting) then observes the re-freeze
			// (the env is Running/Freezing by then, not the stale Waiting).
			h.WaitForWakeCount(ctx, key, int32(cycle))
			h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)
			expectNoLeakedResources(ctx, ns, env)
		}

		final := h.GetEnv(ctx, key)
		Expect(final.Status.WakeCount).To(Equal(int32(3)))
		Expect(final.Status.FreezeCount).To(Equal(int32(4)))
		expectNoLeakedResources(ctx, ns, env)
	})
})
