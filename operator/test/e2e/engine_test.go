package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/klauspost/compress/zstd"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
	"github.com/psenna/ai-sandbox/operator/internal/sandboxctl"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// findContainer returns the container (regular or init) named name in pod,
// or nil if none matches.
func findContainer(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

// readSnapshotTar zstd-decompresses raw and reads it as a tar stream,
// returning every entry's name (in archive order) and, for regular files,
// its content -- read directly into memory rather than via
// storage.ReadArchive/RestoreFrom (which extract to a filesystem), since
// these specs only need to LOOK at what a snapshot contains.
func readSnapshotTar(raw []byte) (names []string, files map[string][]byte) {
	zr, err := zstd.NewReader(bytes.NewReader(raw))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	defer zr.Close()

	files = map[string][]byte{}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		names = append(names, hdr.Name)
		if hdr.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(tr)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			files[hdr.Name] = body
		}
	}
	return names, files
}

// waitForEventReason polls ns for a corev1.Event on the object identified by
// uid with the given reason -- the same List-and-filter idiom
// metrics_test.go's own Event assertions use, needed because the event
// broadcaster delivers Eventf calls through a buffered async flush loop, so
// a single point-in-time List is not enough.
func waitForEventReason(ctx context.Context, ns string, uid types.UID, reason string, timeout time.Duration) {
	EventuallyWithOffset(1, func() bool {
		var eventsList corev1.EventList
		if err := h.Client.List(ctx, &eventsList, client.InNamespace(ns)); err != nil {
			return false
		}
		for _, e := range eventsList.Items {
			if e.InvolvedObject.UID == uid && e.Reason == reason {
				return true
			}
		}
		return false
	}, timeout, h.Cfg.Poll).Should(BeTrue(), "namespace %s never recorded an Event with reason %s for object %s", ns, reason, uid)
}

var _ = Describe("rootless-podman engine", Ordered, func() {
	var (
		ctx context.Context
		ns  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		ns = h.NewNamespace(ctx, "e2e-engine")
	})

	It("renders a podman sidecar with exactly the spike's securityContext and no token", func() {
		class := h.CreateClass(ctx, WithPodmanEngine())
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript("SCRIPT:sleep 60"))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseRunning, h.Cfg.PhaseTimeout)
		h.WaitForCondition(ctx, key, "EngineSecurityRelaxed", metav1.ConditionTrue, "EngineRelaxationApplied", h.Cfg.PhaseTimeout)

		pod := h.GetAgentPod(ctx, key)

		Expect(pod.Spec.InitContainers).To(HaveLen(2), "expected exactly [podman, sandboxctl] init containers")
		Expect(pod.Spec.InitContainers[0].Name).To(Equal(render.PodmanContainerName))
		Expect(pod.Spec.InitContainers[1].Name).To(Equal(render.SidecarContainerName))

		podman := findContainer(pod, render.PodmanContainerName)
		Expect(podman).NotTo(BeNil())
		sc := podman.SecurityContext
		Expect(sc).NotTo(BeNil())
		Expect(sc.AppArmorProfile).NotTo(BeNil())
		Expect(sc.AppArmorProfile.Type).To(Equal(corev1.AppArmorProfileTypeUnconfined))
		Expect(sc.SeccompProfile).NotTo(BeNil())
		Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeUnconfined))
		Expect(sc.AllowPrivilegeEscalation).NotTo(BeNil())
		Expect(*sc.AllowPrivilegeEscalation).To(BeTrue())
		Expect(sc.Capabilities).To(BeNil(), "the podman sidecar must not set capabilities at all -- see engine_podman.go's podmanSecurityContext doc comment")
		Expect(sc.Privileged).To(BeNil())

		var mountNames []string
		for _, m := range podman.VolumeMounts {
			mountNames = append(mountNames, m.Name)
		}
		// "workspace" / "podman-graph" are render.go's unexported
		// workspaceVolumeName/podmanGraphVolumeName literal values.
		Expect(mountNames).To(ConsistOf("workspace", "podman-graph"))

		agent := findContainer(pod, render.AgentContainerName)
		Expect(agent).NotTo(BeNil())
		asc := agent.SecurityContext
		Expect(asc).NotTo(BeNil())
		Expect(asc.AllowPrivilegeEscalation).NotTo(BeNil())
		Expect(*asc.AllowPrivilegeEscalation).To(BeFalse(), "the agent container's securityContext must be UNCHANGED by this engine")
		Expect(asc.Capabilities).NotTo(BeNil())
		Expect(asc.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
		Expect(asc.SeccompProfile).NotTo(BeNil())
		Expect(asc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))

		for _, v := range pod.Spec.Volumes {
			Expect(v.HostPath).To(BeNil(), "volume %s must not be a hostPath", v.Name)
		}
	})

	It("runs a container, bind-mounts the workspace, and starts a postgres service container", func() {
		class := h.CreateClass(ctx, WithPodmanEngine())
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:docker version",
			"SCRIPT:docker run --rm -v /workspace:/work -w /work docker.io/library/alpine:3.21 sh -c 'echo from-workload > /work/from-workload.txt'",
			"SCRIPT:require-file /workspace/from-workload.txt",
			"SCRIPT:require-owned /workspace/from-workload.txt",
			"SCRIPT:docker network create e2e-net",
			"SCRIPT:docker run -d --name pg --network e2e-net -e POSTGRES_PASSWORD=e2e -e POSTGRES_USER=e2e -e POSTGRES_DB=e2e docker.io/library/postgres:18-alpine",
			"SCRIPT:docker-eventually 180 exec pg pg_isready -U e2e -d e2e",
			"SCRIPT:docker run --rm --network e2e-net -e PGPASSWORD=e2e docker.io/library/postgres:18-alpine psql -h pg -U e2e -d e2e -c 'select 1'",
			"SCRIPT:sandbox-done success ok",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.ExpectAgentExitCode(ctx, key, 0)
	})

	It("installs an npm package through dependaproxy from inside a workload container", func() {
		class := h.CreateClass(ctx, WithPodmanEngine(), WithDependaProxyNpm(h.NpmRegistryInClusterURL()))
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			`SCRIPT:write package.json {"name":"e2e-npm-workload","version":"1.0.0"}`,
			"SCRIPT:docker run --rm -u 0 -v /workspace:/work -w /work -e NPM_CONFIG_REGISTRY docker.io/library/node:22-alpine sh -c 'npm install --no-audit --no-fund @e2e/hello@1.0.0 && npm ci --no-audit --no-fund'",
			"SCRIPT:require-file /workspace/node_modules/@e2e/hello/index.js",
			"SCRIPT:sandbox-done success ok",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseDone, h.Cfg.PhaseTimeout)
		h.ExpectAgentExitCode(ctx, key, 0)
	})

	It("the layer cache never appears in a snapshot", func() {
		class := h.CreateClass(ctx, WithPodmanEngine())
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:docker pull docker.io/library/alpine:3.21",
			"SCRIPT:write hello.txt from-e2e",
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"e2e layer cache check","params":{"duration":"1h"}}`,
			"SCRIPT:sleep 300",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		// Freezing -> Waiting requires a real, successful snapshot; the
		// exact-mount-set proof that the podman-graph volume is mounted by
		// no container other than podman is already covered, unconditional
		// of what script runs, by "renders a podman sidecar..." above --
		// no need to re-fetch the (about to be deleted) agent pod here too.
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		got := h.GetEnv(ctx, key)
		Expect(got.Status.Snapshot).NotTo(BeNil())
		layout := envLayout(got)
		at := got.Status.Snapshot.TakenAt.Time

		manifestBytes := h.WaitForS3Object(ctx, h.Cfg.SnapshotBucket, layout.Manifest(0, at), h.Cfg.PodTimeout)
		m, err := storage.ParseManifest(bytes.NewReader(manifestBytes))
		Expect(err).NotTo(HaveOccurred())

		wantFiles := map[string]bool{storage.FileWorkspace: false, storage.FileAgentHome: false}
		for _, f := range m.Files {
			Expect(wantFiles).To(HaveKey(f.Name), "manifest.json listed an unexpected file %s", f.Name)
			wantFiles[f.Name] = true
		}
		for name, seen := range wantFiles {
			Expect(seen).To(BeTrue(), "manifest.json should have listed %s", name)
		}

		forbidden := []string{"containers/storage", ".local/share/containers", "overlay/"}
		for _, f := range m.Files {
			body, err := h.S3Get(ctx, h.Cfg.SnapshotBucket, layout.SnapshotFile(0, at, f.Name))
			Expect(err).NotTo(HaveOccurred(), "fetching manifest-listed file %s", f.Name)
			names, _ := readSnapshotTar(body)
			for _, entry := range names {
				for _, bad := range forbidden {
					Expect(entry).NotTo(ContainSubstring(bad), "snapshot file %s contains a layer-cache entry %q", f.Name, entry)
				}
			}
		}
	})

	It("tears down workload containers before the freeze archives the workspace", func() {
		class := h.CreateClass(ctx, WithPodmanEngine())
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript(
			"SCRIPT:docker run -d --name sleeper docker.io/library/alpine:3.21 sleep 600",
			`SCRIPT:sandbox-wait {"type":"NotBefore","reason":"e2e teardown check","params":{"duration":"1h"}}`,
			"SCRIPT:sleep 300",
		))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		// Reaching Waiting at all is itself proof teardown did not error:
		// internal/sandboxctl/snapshot.go's Freeze returns
		// SnapshotReasonTeardownFailed and never archives on a Teardown
		// error, which would leave the environment stuck in Freezing or
		// move it to Failed, never Waiting.
		h.WaitForPhase(ctx, key, sandboxv1alpha1.PhaseWaiting, h.Cfg.PhaseTimeout)

		got := h.GetEnv(ctx, key)
		Expect(got.Status.Snapshot).NotTo(BeNil())
		layout := envLayout(got)
		at := got.Status.Snapshot.TakenAt.Time

		wsBytes, err := h.S3Get(ctx, h.Cfg.SnapshotBucket, layout.Workspace(0, at))
		Expect(err).NotTo(HaveOccurred())

		_, files := readSnapshotTar(wsBytes)
		markerBytes, ok := files[".sandbox/last-freeze.json"]
		Expect(ok).To(BeTrue(), "workspace archive should contain .sandbox/last-freeze.json")

		var marker sandboxctl.TeardownMarker
		Expect(json.Unmarshal(markerBytes, &marker)).To(Succeed())
		Expect(marker.Engine).To(Equal(string(sandboxv1alpha1.EngineTypeRootlessPodman)))
		Expect(marker.Destroyed.Containers).To(ContainElement("sleeper"))
		Expect(marker.Destroyed.ImageLayerCache).To(BeTrue())
	})

	It("reports an actionable error for a rootless-podman class in a restricted namespace", func() {
		// Labelled AFTER creation, deliberately -- exactly the "someone
		// edited the label later" case the Helm chart's install-time guard
		// G6 cannot see (it only inspects values at template time).
		var namespace corev1.Namespace
		Expect(h.Client.Get(ctx, client.ObjectKey{Name: ns}, &namespace)).To(Succeed())
		if namespace.Labels == nil {
			namespace.Labels = map[string]string{}
		}
		namespace.Labels["pod-security.kubernetes.io/enforce"] = "restricted"
		Expect(h.Client.Update(ctx, &namespace)).To(Succeed())

		class := h.CreateClass(ctx, WithPodmanEngine())
		env := h.CreateEnvironment(ctx, ns, class.Name, WithScript("SCRIPT:sleep 30"))
		key := client.ObjectKey{Namespace: ns, Name: env.Name}

		h.WaitForCondition(ctx, key, "EngineSecurityRelaxed", metav1.ConditionUnknown, "NamespacePodSecurityIncompatible", h.Cfg.PhaseTimeout)
		Expect(conditionMessage(h.GetEnv(ctx, key), "EngineSecurityRelaxed")).To(ContainSubstring("pod-security.kubernetes.io/enforce"))

		got := h.GetEnv(ctx, key)
		waitForEventReason(ctx, ns, got.UID, "EngineNamespaceIncompatible", h.Cfg.PhaseTimeout)

		Consistently(func() bool {
			return h.getAgentPodOrNil(ctx, key) == nil
		}, 15*time.Second, h.Cfg.Poll).Should(BeTrue(), "no agent pod should ever be created for a namespace-incompatible engine")
	})
})
