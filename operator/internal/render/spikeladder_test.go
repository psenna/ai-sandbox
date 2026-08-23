package render

import (
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// ladderDoc is the subset of a plain Kubernetes Pod manifest this test
// needs: enough to find the `podman` container's image and securityContext
// without pulling in a full typed decoder.
type ladderDoc struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec corev1.PodSpec `json:"spec"`
}

// parseLadderDocs reads path (a repo-relative path, mirroring
// internal/docs/reasons_test.go's "test reads a repo file two directories
// up" idiom for ../../docs/operations.md) and returns every Pod document in
// it, in file order.
func parseLadderDocs(t *testing.T, path string) []ladderDoc {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is always one of the two hardcoded spike YAML paths below
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var docs []ladderDoc
	for _, chunk := range strings.Split(string(raw), "\n---\n") {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		var d ladderDoc
		if err := yaml.Unmarshal([]byte(chunk), &d); err != nil {
			t.Fatalf("parsing a document in %s: %v", path, err)
		}
		if d.Kind != "Pod" {
			continue
		}
		docs = append(docs, d)
	}
	return docs
}

// podmanImageFrom finds the `podman` container's image in a parsed ladder
// Pod.
func podmanImageFrom(t *testing.T, doc ladderDoc) string {
	t.Helper()
	for _, c := range doc.Spec.Containers {
		if c.Name == "podman" {
			return c.Image
		}
	}
	t.Fatalf("pod %q has no podman container", doc.Metadata.Name)
	return ""
}

// TestSpikeLadder_ImagesMatchPinnedEngineImage proves the spike YAML files
// were kept in sync with DefaultPodmanImage: every ladder pod's (and the
// topology pod's podman container's) image equals the constant this engine
// actually renders.
func TestSpikeLadder_ImagesMatchPinnedEngineImage(t *testing.T) {
	const failMsg = "the podman image was bumped without re-running the #23 privilege ladder -- see spike/podman-privilege-ladder.yaml's header for the exact procedure."

	for _, doc := range parseLadderDocs(t, "../../spike/podman-privilege-ladder.yaml") {
		got := podmanImageFrom(t, doc)
		if got != DefaultPodmanImage {
			t.Errorf("%s\npod %q image = %q, want %q", failMsg, doc.Metadata.Name, got, DefaultPodmanImage)
		}
	}

	topology := parseLadderDocs(t, "../../spike/podman-topology.yaml")
	if len(topology) != 1 {
		t.Fatalf("spike/podman-topology.yaml: got %d Pod documents, want 1", len(topology))
	}
	if got := podmanImageFrom(t, topology[0]); got != DefaultPodmanImage {
		t.Errorf("%s\ntopology pod image = %q, want %q", failMsg, got, DefaultPodmanImage)
	}
}

// TestSpikeLadder_RenderedSidecarMatchesTheOnlyWorkingCase renders a
// rootless-podman pod and asserts the podman sidecar's securityContext
// matches ladder case G (pm-apparmor) -- the only case that actually works.
func TestSpikeLadder_RenderedSidecarMatchesTheOnlyWorkingCase(t *testing.T) {
	pod, err := RenderPod(Inputs{Env: baseEnv("spikeladder-g"), Class: podmanClass(), ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	sc := podmanContainerOf(t, pod).SecurityContext

	if sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
		t.Errorf("RunAsUser = %v, want 1000 (ladder case G, pm-apparmor)", sc.RunAsUser)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != 1000 {
		t.Errorf("RunAsGroup = %v, want 1000 (ladder case G, pm-apparmor)", sc.RunAsGroup)
	}
	if sc.AppArmorProfile == nil || sc.AppArmorProfile.Type == nil || *sc.AppArmorProfile.Type != "Unconfined" {
		t.Errorf("AppArmorProfile = %+v, want type Unconfined (ladder case G, pm-apparmor)", sc.AppArmorProfile)
	}
	if sc.Capabilities != nil {
		t.Errorf("Capabilities = %+v, want nil (case G adds none and drops none)", sc.Capabilities)
	}
	if sc.Privileged != nil {
		t.Errorf("Privileged = %v, want nil", sc.Privileged)
	}
}

// TestSpikeLadder_RenderedSidecarMatchesNoFailingCase proves the rendered
// sidecar's securityContext never regresses toward any of the ladder's
// FAILING distinguishing fields.
func TestSpikeLadder_RenderedSidecarMatchesNoFailingCase(t *testing.T) {
	pod, err := RenderPod(Inputs{Env: baseEnv("spikeladder-fail"), Class: podmanClass(), ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	sc := podmanContainerOf(t, pod).SecurityContext

	t.Run("pm-aa-seccomp (ladder case H)", func(t *testing.T) {
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type == nil || *sc.SeccompProfile.Type != "Unconfined" {
			t.Errorf("SeccompProfile = %+v, want type Unconfined (not RuntimeDefault, not nil) -- reproducing ladder case H (pm-aa-seccomp) would deny clone(CLONE_NEWUSER)", sc.SeccompProfile)
		}
	})
	t.Run("pm-aa-noprivesc (ladder case J)", func(t *testing.T) {
		if sc.AllowPrivilegeEscalation == nil || !*sc.AllowPrivilegeEscalation {
			t.Errorf("AllowPrivilegeEscalation = %v, want true -- reproducing ladder case J (pm-aa-noprivesc) would void newuidmap's cap_setuid file capability", sc.AllowPrivilegeEscalation)
		}
	})
	t.Run("pm-sysadmin/pm-sysadmin-root (ladder cases C/E)", func(t *testing.T) {
		if sc.Capabilities != nil {
			t.Errorf("Capabilities = %+v, want nil -- reproducing ladder cases C/E (pm-sysadmin, pm-sysadmin-root) adds capabilities that do not help and widens the blast radius", sc.Capabilities)
		}
	})
	t.Run("pm-privileged (ladder case D)", func(t *testing.T) {
		if sc.Privileged != nil {
			t.Errorf("Privileged = %v, want nil -- reproducing ladder case D (pm-privileged) is far more privilege than necessary", sc.Privileged)
		}
	})
}

// TestSpikeLadder_TopologyPodMatchesRenderedTopology cross-checks the
// topology manifest's podman container command, the workspace mount path,
// and the graph mount path against what RenderPod actually produces.
func TestSpikeLadder_TopologyPodMatchesRenderedTopology(t *testing.T) {
	docs := parseLadderDocs(t, "../../spike/podman-topology.yaml")
	if len(docs) != 1 {
		t.Fatalf("spike/podman-topology.yaml: got %d Pod documents, want 1", len(docs))
	}
	var topologyPodman *corev1.Container
	for i := range docs[0].Spec.Containers {
		if docs[0].Spec.Containers[i].Name == "podman" {
			topologyPodman = &docs[0].Spec.Containers[i]
		}
	}
	if topologyPodman == nil {
		t.Fatal("topology pod has no podman container")
	}

	pod, err := RenderPod(Inputs{Env: baseEnv("spikeladder-topology"), Class: podmanClass(), ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	rendered := podmanContainerOf(t, pod)

	wantCommand := strings.Join(topologyPodman.Command, " ")
	if !strings.Contains(rendered.Args[0], "exec "+wantCommand) {
		t.Errorf("rendered bootstrap script does not exec the topology's command %q:\n%s", wantCommand, rendered.Args[0])
	}

	var topologyWorkspacePath, topologyGraphPath string
	for _, m := range topologyPodman.VolumeMounts {
		switch m.Name {
		case "workspace":
			topologyWorkspacePath = m.MountPath
		case "layer-cache":
			topologyGraphPath = m.MountPath
		}
	}
	if topologyWorkspacePath != WorkspaceMountPath {
		t.Errorf("topology workspace mount path = %q, want %q", topologyWorkspacePath, WorkspaceMountPath)
	}
	if topologyGraphPath != PodmanGraphMountPath {
		t.Errorf("topology graph mount path = %q, want %q (render.PodmanGraphMountPath)", topologyGraphPath, PodmanGraphMountPath)
	}

	var gotWorkspacePath, gotGraphPath string
	for _, m := range rendered.VolumeMounts {
		if m.Name == nil || m.MountPath == nil {
			continue
		}
		switch *m.Name {
		case workspaceVolumeName:
			gotWorkspacePath = *m.MountPath
		case podmanGraphVolumeName:
			gotGraphPath = *m.MountPath
		}
	}
	if gotWorkspacePath != WorkspaceMountPath {
		t.Errorf("rendered workspace mount path = %q, want %q", gotWorkspacePath, WorkspaceMountPath)
	}
	if gotGraphPath != PodmanGraphMountPath {
		t.Errorf("rendered graph mount path = %q, want %q", gotGraphPath, PodmanGraphMountPath)
	}
}
