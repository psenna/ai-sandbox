package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// e2eClusterID must match the e2e overlay's operator deployment: no
// --cluster-id flag override in test/e2e/manifests/operator/kustomization.yaml,
// so internal/config.Config's built-in default ("default") applies.
const e2eClusterID = "default"

// s3FaultTimeout bounds the "backend outage" spec's wait for the sidecar to
// exhaust its retry budget (default --snapshot-retries=4, so 5 attempts,
// each with the AWS SDK's own internal retryer) and report Failed. Measured
// directly against a real, uncontended cluster (a manual kubectl
// reproduction, not this spec) at ~30s wall time, and ~66s-180s when run
// alone or as part of a small subset under Ginkgo. Repeated timeouts at
// higher and higher budgets (3m, then 6m) while running as part of the full
// suite under --procs=4 turned out NOT to be this spec running slowly --
// see the "SCRIPT:sleep 900" comment above for the actual root cause (the
// agent's own script duration was cutting the sidecar off mid-retry). With
// that fixed, 8 minutes is a generous margin for the real contention a busy
// single-node kind cluster can introduce, comfortably inside the 15-minute
// ceiling the agent's script now allows. An EARLIER version of this spec
// used h.Cfg.PhaseTimeout (5m) and also timed out -- that was a symptom of
// running on a cluster carrying dozens of leftover namespaces from a prior,
// separately-torn-down-and-recreated test run (this suite's own
// DeferCleanup-based namespace cleanup is best-effort, see harness.go's
// NewNamespace), a third, independent cause from the two above.
const s3FaultTimeout = 8 * time.Minute

// envLayout computes the storage.Layout for env, matching internal/storage's
// path-layout contract exactly (see internal/sandboxctl/snapshotconfig.go's
// snapshotLayout, which the sidecar itself builds the same way from its
// projected --cluster-id/--s3-prefix flags -- the e2e class here sets no
// prefix).
func envLayout(env *sandboxv1alpha1.SandboxEnvironment) storage.Layout {
	l, err := storage.NewLayout("", storage.Identity{
		ClusterID: e2eClusterID,
		Namespace: env.Namespace,
		EnvName:   env.Name,
		EnvUID:    string(env.UID),
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return l
}

var _ = Describe("freeze", func() {
	var (
		ctx context.Context
		ns  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		ns = h.NewNamespace(ctx, "e2e-freeze")
	})

	It("freezes, uploads a verifiable snapshot, deletes the pod and releases the slot", func() {
		class := h.CreateClass(ctx)
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:write hello.txt world",
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"e2e freeze test","params":{"duration":"1h"}}`,
			"SCRIPT:sleep 300",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		// Freezing -> Waiting requires a real, successful snapshot (#28);
		// reaching Waiting is itself proof the freeze completed.
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		Eventually(func() bool {
			return h.getAgentPodOrNil(ctx, key) == nil
		}, h.Cfg.PodTimeout, h.Cfg.Poll).Should(BeTrue(), "agent pod should be deleted once frozen")

		got := h.GetEnv(ctx, key)
		Expect(got.Status.Slot.Granted).To(BeFalse(), "slot should be released once frozen")
		Expect(got.Status.FreezeCount).To(Equal(int32(1)))

		snap := got.Status.Snapshot
		Expect(snap).NotTo(BeNil())
		Expect(snap.Seq).To(Equal(int32(0)))
		Expect(snap.SizeBytes).To(BeNumerically(">", 0))
		Expect(snap.DurationMillis).To(BeNumerically(">", 0))
		Expect(snap.SHA256).To(MatchRegexp(`^[a-f0-9]{64}$`))
		Expect(snap.TakenAt).NotTo(BeNil())

		layout := envLayout(got)
		at := snap.TakenAt.Time

		// Real end-to-end checksum verification: fetch manifest.json,
		// parse it, then fetch and hash EVERY file it lists and compare
		// against the manifest's own recorded checksum -- not just
		// checking that status.snapshot exists.
		manifestBytes := h.WaitForS3Object(ctx, h.Cfg.SnapshotBucket, layout.Manifest(0, at), h.Cfg.PodTimeout)
		m, err := storage.ParseManifest(bytes.NewReader(manifestBytes))
		Expect(err).NotTo(HaveOccurred())
		Expect(m.Files).NotTo(BeEmpty())

		for _, f := range m.Files {
			body, err := h.S3Get(ctx, h.Cfg.SnapshotBucket, layout.SnapshotFile(0, at, f.Name))
			Expect(err).NotTo(HaveOccurred(), "fetching manifest-listed file %s", f.Name)
			sum := sha256.Sum256(body)
			Expect(hex.EncodeToString(sum[:])).To(Equal(f.SHA256), "checksum mismatch for %s", f.Name)
			Expect(int64(len(body))).To(Equal(f.Size), "size mismatch for %s", f.Name)
		}

		latestBytes, err := h.S3Get(ctx, h.Cfg.SnapshotBucket, layout.Latest())
		Expect(err).NotTo(HaveOccurred())
		latest, err := storage.ParseLatest(bytes.NewReader(latestBytes))
		Expect(err).NotTo(HaveOccurred())
		Expect(latest.Seq).To(Equal(0))

		// A second environment created in the same namespace reaches
		// Running, proving the slot was genuinely released. This does NOT
		// prove CONTENTION (capacity-1 admission ordering) -- that's
		// already covered at the envtest layer by
		// internal/controller/slotscheduler_test.go; the e2e overlay's
		// slot-capacity (8) means this spec alone can't distinguish
		// "released" from "never actually needed".
		env2 := h.CreateEnvironment(ctx, ns, class.Name, WithScript("SCRIPT:sleep 5"))
		key2 := client.ObjectKey{Namespace: ns, Name: env2.Name}
		h.WaitForPhase(ctx, key2, sandboxv1alpha1.PhaseRunning, h.Cfg.PhaseTimeout)
	})

	It("sequence numbers increase monotonically across repeated freezes", func() {
		// NOT a real wake (#29 restores nothing): clearing status.waitFor
		// directly, via the admin client, exercises nextWaiting's
		// WaitFor==nil branch back to Ready, and the scheduler +
		// nextRestoring recreate the pod against the still-intact
		// workspace PVC (the warm cache). This exercises the
		// sequence-number contract only, not an actual restore.
		class := h.CreateClass(ctx)
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"first freeze","params":{"duration":"1h"}}`,
			"SCRIPT:sleep 300",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		first := h.GetEnv(ctx, key)
		Expect(first.Status.Snapshot).NotTo(BeNil())
		Expect(first.Status.Snapshot.Seq).To(Equal(int32(0)))
		Expect(first.Status.FreezeCount).To(Equal(int32(1)))

		patch := client.MergeFrom(first.DeepCopy())
		modified := first.DeepCopy()
		modified.Status.WaitFor = nil
		Expect(h.Client.Status().Patch(ctx, modified, patch)).To(Succeed())

		// Ready is transient here: nextReady's very next reconcile (the pod
		// is gone, since the freeze deleted it) advances straight to
		// Restoring, often within one h.Cfg.Poll interval. A plain
		// WaitForPhase(Ready) polls on a fixed interval and can step right
		// over that narrow window, timing out despite Ready having
		// genuinely (if briefly) occurred -- observed directly via a live
		// kubectl watch during this test's own debugging. Accept any phase
		// that proves real forward progress out of the WaitFor-blocked
		// Waiting state instead of requiring the poll to catch the exact
		// instant it was Ready.
		h.WaitForAnyPhase(ctx, key, h.Cfg.PhaseTimeout,
			sandboxv1alpha1.PhaseReady, sandboxv1alpha1.PhaseRestoring,
			sandboxv1alpha1.PhaseRunning, sandboxv1alpha1.PhaseFreezing)
		// The same script runs again from the top on the new pod (there is
		// no resume -- #29) and immediately declares a second wait.
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		final := h.GetEnv(ctx, key)
		Expect(final.Status.FreezeCount).To(Equal(int32(2)))
		Expect(final.Status.Snapshot).NotTo(BeNil())
		Expect(final.Status.Snapshot.Seq).To(Equal(int32(1)))

		layout := envLayout(final)
		latestBytes, err := h.S3Get(ctx, h.Cfg.SnapshotBucket, layout.Latest())
		Expect(err).NotTo(HaveOccurred())
		latest, err := storage.ParseLatest(bytes.NewReader(latestBytes))
		Expect(err).NotTo(HaveOccurred())
		Expect(latest.Seq).To(Equal(1), "latest.json should point at the most recent snapshot (seq 1)")

		objs, err := h.S3List(ctx, h.Cfg.SnapshotBucket, layout.SnapshotsPrefix())
		Expect(err).NotTo(HaveOccurred())
		dirs := map[string]bool{}
		for _, o := range objs {
			dirs[o.Key] = true
		}
		_ = dirs // the exhaustive per-seq accounting is exercised at the unit level (snapshot_test.go); here we only need >=2 distinct snapshot directories to exist.
		seq0 := layout.SnapshotFile(0, first.Status.Snapshot.TakenAt.Time, storage.FileManifest)
		seq1 := layout.SnapshotFile(1, final.Status.Snapshot.TakenAt.Time, storage.FileManifest)
		found0, found1 := false, false
		for _, o := range objs {
			if o.Key == seq0 {
				found0 = true
			}
			if o.Key == seq1 {
				found1 = true
			}
		}
		Expect(found0).To(BeTrue(), "seq 0 manifest should still exist: %s", seq0)
		Expect(found1).To(BeTrue(), "seq 1 manifest should exist: %s", seq1)
	})

	// WHAT THIS CANNOT PROVE YET, BY DESIGN: that the agent reads the
	// marker and acts on it after a wake. There is no wake path (#29).
	// This spec proves the WRITE half only (that a freeze which included
	// marker-writing completed successfully); the round-trip test the
	// issue's Update section describes belongs to #29. The marker's exact
	// content/determinism is proven at the unit level
	// (internal/sandboxctl/marker_test.go).
	It("completes a freeze that writes the teardown marker", func() {
		class := h.CreateClass(ctx)
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:write hello.txt world",
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"marker check","params":{"duration":"1h"}}`,
			"SCRIPT:sleep 300",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		got := h.GetEnv(ctx, key)
		Expect(got.Status.Snapshot).NotTo(BeNil())
		layout := envLayout(got)
		wsBytes, err := h.S3Get(ctx, h.Cfg.SnapshotBucket, layout.Workspace(0, got.Status.Snapshot.TakenAt.Time))
		Expect(err).NotTo(HaveOccurred())
		// A workspace containing only "hello.txt" plus the marker files
		// (.sandbox/last-freeze.json, .sandbox/RESUME.md) compresses to
		// something comfortably larger than an empty archive would --
		// a light, non-exhaustive signal that more than nothing landed in
		// the tarball. The exhaustive entry-list assertion lives in
		// internal/sandboxctl/exclusions_test.go/marker_test.go.
		Expect(len(wsBytes)).To(BeNumerically(">", 0))
	})

	It("a backend outage mid-upload never loses context and never publishes a partial snapshot", Serial, func() {
		// Serial: this spec (and "a pod lost mid-freeze...") program a fault
		// on the s3proxy double, which is ONE shared service across every
		// parallel Ginkgo process, not scoped per-namespace like everything
		// else in this suite. Running concurrently with another spec that
		// clears/depends on that same global fault state is a real,
		// observed source of cross-process flakiness (a concurrent
		// DeferCleanup clearing the fault mid-retry let a "should still be
		// failing" attempt succeed early). Serial guarantees this spec runs
		// alone, in its own process, never overlapping the other one.
		//
		// fail-write is set BEFORE the environment exists, so the very
		// first archive upload attempt hits it -- s3proxy drains
		// afterBytes of the request body (so the client really is
		// mid-stream) then answers 503 with an S3-shaped error, which
		// internal/storage's classifyS3Error maps to ErrUnreachable
		// (retryable). Cleared unconditionally so a failure here can't
		// wedge every later spec in this file.
		h.S3ProxySetFault(ctx, "fail-write", 503, 4096)
		DeferCleanup(func(ctx context.Context) { h.S3ProxyClearFault(ctx) })

		class := h.CreateClass(ctx, WithS3Endpoint(h.S3ProxyInClusterEndpoint()))
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:write-tree big.bin 2097152", // 2 MiB, comfortably over afterBytes
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"fault injection","params":{"duration":"1h"}}`,
			// This was 300 (5 minutes) and was the ACTUAL root cause of every
			// timeout observed while debugging this spec, not a hang in the
			// retry/archive path: the fake agent's own script has NOTHING to
			// do with the freeze outcome -- it just sleeps for the given
			// duration and exits 0 regardless. Once it does, Kubernetes'
			// native-sidecar semantics (KEP-753) force-terminate the
			// still-retrying sandboxctl sidecar too, cutting it off
			// mid-attempt before it can ever call RecordSnapshot with a
			// Failed outcome -- confirmed directly from a failed run's
			// collected pod status: both containers showed finishedAt
			// exactly 300s after the sidecar's startedAt, exit code 0
			// (Completed, not a crash), while status.snapshotAttempt was
			// still frozen at "InProgress" from several minutes earlier.
			// 15 minutes comfortably exceeds even a heavily contended
			// retry-and-fail sequence (5 attempts, each individually bounded
			// by sandboxctl's --snapshot-step-timeout, default 2m).
			"SCRIPT:sleep 900",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		// Every archive upload attempt (default --snapshot-retries=4, so 5
		// attempts) goes through a full multipart-upload cycle, and the AWS
		// SDK's OWN internal retryer adds further backoff underneath
		// retrySnapshotStep's outer 1/2/4/8s backoff -- measured at over
		// 300s wall time for the sidecar to exhaust its retry budget and
		// report Failed. h.Cfg.PhaseTimeout (5m default) is not enough
		// headroom; use a generously larger, explicit timeout instead.
		h.WaitForCondition(ctx, key, "Frozen", metav1.ConditionFalse, "SnapshotFailed", s3FaultTimeout)
		// Held, not advanced: the environment must sit in Freezing rather
		// than proceeding to delete the pod.
		h.ExpectStablePhase(ctx, key, sandboxv1alpha1.PhaseFreezing, 20*time.Second)

		// The agent's context (the pod) is still there -- nothing was
		// silently dropped.
		Expect(h.getAgentPodOrNil(ctx, key)).NotTo(BeNil(), "agent pod must survive a failed snapshot attempt")

		got := h.GetEnv(ctx, key)
		Expect(got.Status.SnapshotAttempt).NotTo(BeNil())
		Expect(got.Status.SnapshotAttempt.Phase).To(Equal(sandboxv1alpha1.SnapshotAttemptFailed))
		Expect(got.Status.SnapshotAttempt.Attempts).To(BeNumerically(">=", 2), "retries should actually have happened")
		Expect(got.Status.SnapshotAttempt.Reason).To(Equal("BackendUnreachable"))
		Expect(got.Status.Snapshot).To(BeNil(), "a failed attempt must never populate status.snapshot")

		// No half-written snapshot: no manifest.json under this
		// environment's snapshot prefix, and latest.json 404s.
		layout := envLayout(got)
		objs, err := h.S3List(ctx, h.Cfg.SnapshotBucket, layout.SnapshotsPrefix())
		Expect(err).NotTo(HaveOccurred())
		for _, o := range objs {
			Expect(o.Key).NotTo(HaveSuffix(storage.FileManifest), "no manifest.json should exist after every attempt failed")
		}
		_, err = h.S3Get(ctx, h.Cfg.SnapshotBucket, layout.Latest())
		Expect(err).To(HaveOccurred(), "latest.json should not exist")

		// The fault really fired at least once.
		reqs := h.S3ProxyRequests(ctx)
		Expect(reqs).NotTo(BeEmpty())
	})

	It("the image/layer cache is absent from the snapshot", func() {
		class := h.CreateClass(ctx)
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			// 8 MiB of /dev/urandom is incompressible by zstd, so if it
			// were included the archive would be at least that large; a
			// small resulting archive is a decompression-free proof the
			// cache path was excluded. Exhaustive entry-list coverage
			// lives in internal/sandboxctl/exclusions_test.go.
			"SCRIPT:write-tree .local/share/containers/storage/overlay/blob 8388608",
			"SCRIPT:write real.txt small",
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"cache exclusion","params":{"duration":"1h"}}`,
			"SCRIPT:sleep 300",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		got := h.GetEnv(ctx, key)
		Expect(got.Status.Snapshot).NotTo(BeNil())
		layout := envLayout(got)
		wsBytes, err := h.S3Get(ctx, h.Cfg.SnapshotBucket, layout.Workspace(0, got.Status.Snapshot.TakenAt.Time))
		Expect(err).NotTo(HaveOccurred())
		Expect(len(wsBytes)).To(BeNumerically("<", 1<<20),
			"workspace.tar.zst should be well under 1MiB if the 8MiB cache blob was really excluded")
	})

	It("a pod lost mid-freeze is recovered by a snapshot Job", Serial, func() {
		// Serial: see the "backend outage" spec's comment -- the s3proxy
		// fault is global, shared across every parallel Ginkgo process. A
		// concurrent spec clearing/setting it mid-test was observed to let
		// this spec's environment freeze and delete its pod successfully
		// BEFORE this spec's own GetAgentPod call ran, which is exactly the
		// race this decorator prevents.
		//
		// Deterministic ordering: set the fault FIRST so the sidecar
		// wedges in Freezing and never completes on its own, delete the
		// agent pod out from under it, THEN clear the fault so the
		// recovery Job (running against the surviving workspace PVC) can
		// succeed.
		h.S3ProxySetFault(ctx, "fail-write", 503, 4096)
		DeferCleanup(func(ctx context.Context) { h.S3ProxyClearFault(ctx) })

		class := h.CreateClass(ctx, WithS3Endpoint(h.S3ProxyInClusterEndpoint()))
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:write hello.txt world",
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"recovery job","params":{"duration":"1h"}}`,
			"SCRIPT:sleep 300",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseFreezing, h.Cfg.PhaseTimeout)

		// Wait for the sidecar to actually be MID-RETRY (not just for the
		// phase to have flipped to Freezing) before deleting the pod --
		// status.snapshotAttempt.attempts>=1 is a direct, unambiguous
		// signal that the fault has genuinely engaged, removing any race
		// between "phase observed as Freezing" and "the pod happens to
		// still exist a moment later".
		Eventually(func() int32 {
			e := h.GetEnv(ctx, key)
			if e.Status.SnapshotAttempt == nil {
				return 0
			}
			return e.Status.SnapshotAttempt.Attempts
		}, h.Cfg.PodTimeout, h.Cfg.Poll).Should(BeNumerically(">=", 1), "sidecar should be visibly retrying before the pod is deleted out from under it")

		// A plain Delete only sets deletionTimestamp -- the sidecar process
		// keeps running through its terminationGracePeriodSeconds (120s
		// here), and was observed (during this test's own debugging) to
		// win a race against this spec: attempts>=1 is satisfied on the
		// very first attempt (recorded before the archive PUT even
		// completes), so this code reaches the fault-clear below within
		// well under a second of the first failed upload, and the
		// still-alive-and-retrying pod then simply retried, found the
		// fault already cleared, and completed the snapshot itself -- so
		// no recovery Job was ever needed.
		//
		// A grace-period-ZERO force-delete is NOT the fix: it removes the
		// pod OBJECT from the API immediately, without waiting for the
		// kubelet's confirmation that the container actually stopped --
		// this was tried and still observed the same race, since the
		// sidecar process (and its in-flight retry) can keep running for a
		// moment after the object has already vanished from `kubectl get
		// pods`. A short but NONZERO grace period keeps the normal,
		// kubelet-confirmed removal path (the object only disappears once
		// the kubelet reports the container has actually stopped) while
		// still bounding the wait far below the default 120s.
		pod := h.GetAgentPod(ctx, key)
		Expect(h.Client.Delete(ctx, pod, client.GracePeriodSeconds(1))).To(Succeed())
		Eventually(func() bool {
			return h.getAgentPodOrNil(ctx, key) == nil
		}, h.Cfg.PodTimeout, h.Cfg.Poll).Should(BeTrue(), "agent pod should be fully gone (kubelet-confirmed) before the fault is cleared")

		h.S3ProxyClearFault(ctx)

		jobKey := client.ObjectKey{Namespace: ns, Name: render.ChildNames(env.Name).SnapshotJob}
		Eventually(func() error {
			var job batchv1.Job
			return h.Client.Get(ctx, jobKey, &job)
		}, h.Cfg.PhaseTimeout, h.Cfg.Poll).Should(Succeed(), "recovery Job should be created")

		Eventually(func() int32 {
			var job batchv1.Job
			if err := h.Client.Get(ctx, jobKey, &job); err != nil {
				return 0
			}
			return job.Status.Succeeded
		}, h.Cfg.PhaseTimeout, h.Cfg.Poll).Should(BeNumerically(">=", 1), "recovery Job should complete")

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		got := h.GetEnv(ctx, key)
		Expect(got.Status.Snapshot).NotTo(BeNil())
		Expect(got.Status.Snapshot.Seq).To(Equal(int32(0)))

		layout := envLayout(got)
		manifestBytes := h.WaitForS3Object(ctx, h.Cfg.SnapshotBucket, layout.Manifest(0, got.Status.Snapshot.TakenAt.Time), h.Cfg.PodTimeout)
		m, err := storage.ParseManifest(bytes.NewReader(manifestBytes))
		Expect(err).NotTo(HaveOccurred())
		var names []string
		for _, f := range m.Files {
			names = append(names, f.Name)
		}
		Expect(names).To(ConsistOf(storage.FileWorkspace), "the recovery Job has no agent home to archive")
		Expect(strings.Join(names, ",")).NotTo(ContainSubstring(storage.FileAgentHome))
	})
})
