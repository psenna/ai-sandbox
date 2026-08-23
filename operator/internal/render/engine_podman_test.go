package render

import (
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestPodmanEngine_Type(t *testing.T) {
	if got := (podmanEngine{}).Type(); got != v1alpha1.EngineTypeRootlessPodman {
		t.Errorf("podmanEngine{}.Type() = %q, want %q", got, v1alpha1.EngineTypeRootlessPodman)
	}
}

func TestPodmanEngine_ContributeWithZeroInputs(t *testing.T) {
	c, err := podmanEngine{}.Contribute(Inputs{})
	if err != nil {
		t.Fatalf("Contribute(Inputs{}): %v", err)
	}
	if len(c.Relaxations) != 3 {
		t.Errorf("Relaxations = %d, want 3", len(c.Relaxations))
	}
}

func TestPodmanEngine_ExactRelaxationSet(t *testing.T) {
	c, err := podmanEngine{}.Contribute(Inputs{})
	if err != nil {
		t.Fatalf("Contribute: %v", err)
	}
	if len(c.Relaxations) != 3 {
		t.Fatalf("Relaxations = %d, want 3", len(c.Relaxations))
	}
	wantKinds := map[RelaxationKind]bool{
		RelaxAppArmorUnconfined:       true,
		RelaxSeccompUnconfined:        true,
		RelaxAllowPrivilegeEscalation: true,
	}
	gotKinds := map[RelaxationKind]bool{}
	for _, r := range c.Relaxations {
		if r.Container != PodmanContainerName {
			t.Errorf("relaxation %+v: Container = %q, want %q", r, r.Container, PodmanContainerName)
		}
		if r.Container == AgentContainerName || r.Container == SidecarContainerName {
			t.Errorf("relaxation %+v targets a reserved container", r)
		}
		if r.Reason == "" {
			t.Errorf("relaxation %+v has empty Reason", r)
		}
		if !strings.Contains(r.Reason, "spike #23") {
			t.Errorf("relaxation %+v Reason does not mention \"spike #23\"", r)
		}
		gotKinds[r.Kind] = true
	}
	if len(gotKinds) != len(wantKinds) {
		t.Errorf("kinds = %v, want %v", gotKinds, wantKinds)
	}
	for k := range wantKinds {
		if !gotKinds[k] {
			t.Errorf("missing relaxation kind %q", k)
		}
	}
}

func TestPodmanEngine_SidecarSecurityContext(t *testing.T) {
	pod, err := RenderPod(Inputs{Env: baseEnv("podman-sc"), Class: podmanClass(), ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	c := podmanContainerOf(t, pod)
	sc := c.SecurityContext
	if sc == nil {
		t.Fatal("podman container has no SecurityContext")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("RunAsNonRoot = %v, want true", sc.RunAsNonRoot)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != agentUID {
		t.Errorf("RunAsUser = %v, want %d", sc.RunAsUser, agentUID)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != agentGID {
		t.Errorf("RunAsGroup = %v, want %d", sc.RunAsGroup, agentGID)
	}
	if sc.ReadOnlyRootFilesystem == nil || *sc.ReadOnlyRootFilesystem {
		t.Errorf("ReadOnlyRootFilesystem = %v, want false", sc.ReadOnlyRootFilesystem)
	}
	if sc.AllowPrivilegeEscalation == nil || !*sc.AllowPrivilegeEscalation {
		t.Errorf("AllowPrivilegeEscalation = %v, want true", sc.AllowPrivilegeEscalation)
	}
	if sc.AppArmorProfile == nil || sc.AppArmorProfile.Type == nil || *sc.AppArmorProfile.Type != "Unconfined" {
		t.Errorf("AppArmorProfile = %+v, want type Unconfined", sc.AppArmorProfile)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type == nil || *sc.SeccompProfile.Type != "Unconfined" {
		t.Errorf("SeccompProfile = %+v, want type Unconfined", sc.SeccompProfile)
	}
	if sc.Capabilities != nil {
		t.Errorf("Capabilities = %+v, want nil", sc.Capabilities)
	}
	if sc.Privileged != nil {
		t.Errorf("Privileged = %v, want nil", sc.Privileged)
	}
}

func TestPodmanEngine_AgentSecurityContextUnchanged(t *testing.T) {
	nonePod, err := RenderPod(Inputs{Env: baseEnv("agent-sc-none"), Class: withEngine(minimalClass(), v1alpha1.EngineTypeNone), ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
	if err != nil {
		t.Fatalf("RenderPod (none): %v", err)
	}
	podmanPod, err := RenderPod(Inputs{Env: baseEnv("agent-sc-none"), Class: podmanClass(), ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
	if err != nil {
		t.Fatalf("RenderPod (podman): %v", err)
	}
	noneSC := findAgentSecurityContext(t, nonePod)
	podmanSC := findAgentSecurityContext(t, podmanPod)
	got := marshalForGolden(t, podmanSC)
	want := marshalForGolden(t, noneSC)
	if string(got) != string(want) {
		t.Errorf("agent securityContext differs between none and rootless-podman engines:\n--- none ---\n%s\n--- podman ---\n%s", want, got)
	}
}

// TestPodmanEngine_SidecarMountsAndTokensNeverGrow is the tested form of this
// engine's central security argument: the AppArmor relaxation is acceptable
// ONLY because it is confined to a container holding nothing worth stealing.
// That is a maintenance invariant, not a mechanism -- the day someone adds a
// hostPath or a projected token "for convenience", the AGENT inherits it
// through the Docker API, because the agent can `docker run -v <anything the
// sidecar can see>`.
//
// It asserts by EXACT EQUALITY, never by denylist: a denylist passes for
// whatever nobody thought to forbid. Rendered against the MAXIMAL input
// (fullClass + S3 backend + a Restore plan + a registry mirror + credentials),
// so no optional feature can smuggle a mount in.
//
// Deliberately one test asserting every facet of a single security invariant
// together, not fragmented into subtests that could pass individually while
// the invariant as a whole regresses unnoticed.
//
//nolint:gocyclo // see comment above
func TestPodmanEngine_SidecarMountsAndTokensNeverGrow(t *testing.T) {
	class := fullClass()
	class.Spec.Engine.Type = v1alpha1.EngineTypeRootlessPodman
	class.Spec.Services.RegistryMirror = &v1alpha1.RegistryMirrorService{
		URL: "http://registry-cache.ai-sandbox-e2e.svc.cluster.local:5000",
	}
	in := Inputs{
		Env:          baseEnv("podman-maximal", withPrompt("maximal input")),
		Class:        class,
		Credentials:  Credentials{GitProxyToken: "fake-git-proxy-token-podman"}, //nolint:gosec // G101: deliberately fake test fixture value
		ClusterID:    "test-cluster",
		SidecarImage: "test-sidecar:test",
		Restore:      restorePlan(),
	}
	pod, err := RenderPod(in)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	c := podmanContainerOf(t, pod)

	// 1. Exact mount set, in order.
	wantMounts := []struct {
		name     string
		path     string
		readOnly bool
	}{
		{workspaceVolumeName, WorkspaceMountPath, false},
		{podmanGraphVolumeName, PodmanGraphMountPath, false},
	}
	failMsg := "the podman sidecar's mount set changed. Read TestPodmanEngine_SidecarMountsAndTokensNeverGrow's doc comment before you update this list: every mount here is reachable by LLM-authored code through the Docker API."
	if len(c.VolumeMounts) != len(wantMounts) {
		t.Fatalf("%s\ngot %d mounts, want %d: %+v", failMsg, len(c.VolumeMounts), len(wantMounts), c.VolumeMounts)
	}
	for i, want := range wantMounts {
		m := c.VolumeMounts[i]
		gotName, gotPath := "", ""
		if m.Name != nil {
			gotName = *m.Name
		}
		if m.MountPath != nil {
			gotPath = *m.MountPath
		}
		gotRO := m.ReadOnly != nil && *m.ReadOnly
		if gotName != want.name || gotPath != want.path || gotRO != want.readOnly {
			t.Errorf("%s\nmount[%d] = {name:%q path:%q readOnly:%v}, want {name:%q path:%q readOnly:%v}",
				failMsg, i, gotName, gotPath, gotRO, want.name, want.path, want.readOnly)
		}
	}

	// 2. Named, redundant check: none of the sensitive volumes by name.
	forbidden := map[string]bool{
		sidecarTokenVolumeName:        true,
		snapshotCredentialsVolumeName: true,
		configVolumeName:              true,
		agentHomeVolumeName:           true,
	}
	for _, m := range c.VolumeMounts {
		if m.Name != nil && forbidden[*m.Name] {
			t.Errorf("podman sidecar must not mount %q", *m.Name)
		}
	}

	// 3. No envFrom.
	if len(c.EnvFrom) != 0 {
		t.Errorf("podman sidecar EnvFrom = %+v, want empty", c.EnvFrom)
	}

	// 4. Every env var is a literal value, never sourced from a Secret/
	// ConfigMap/field/resource.
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			t.Errorf("podman sidecar env %v has ValueFrom set, want a literal Value", e.Name)
		}
		if e.Value == nil {
			t.Errorf("podman sidecar env %v has no Value", e.Name)
		}
	}

	// 5. Every Contribution-added volume is a bare EmptyDir.
	contribution, err := podmanEngine{}.Contribute(in)
	if err != nil {
		t.Fatalf("Contribute: %v", err)
	}
	for _, v := range contribution.Volumes {
		if v.EmptyDir == nil {
			t.Errorf("contributed volume %v has no EmptyDir", v.Name)
		}
		if v.Projected != nil || v.Secret != nil || v.ConfigMap != nil || v.HostPath != nil || v.CSI != nil || v.DownwardAPI != nil {
			t.Errorf("contributed volume %v has a non-EmptyDir source set: %+v", v.Name, v)
		}
	}

	// 6. Whole-pod sweep: no HostPath volume anywhere.
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			t.Errorf("pod volume %v has HostPath set: %+v", v.Name, v.HostPath)
		}
	}

	// 7. No automounted ServiceAccount token at the pod level.
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("pod.Spec.AutomountServiceAccountToken = %v, want explicit false", pod.Spec.AutomountServiceAccountToken)
	}

	// 8. No containerPort: loopback-only by bind, never a Service.
	if len(c.Ports) != 0 {
		t.Errorf("podman sidecar Ports = %+v, want empty", c.Ports)
	}

	// 9. No privileged, no capabilities on the base securityContext (the
	// relaxations never add Privileged or Capabilities).
	if c.SecurityContext == nil {
		t.Fatal("podman sidecar has no SecurityContext")
	}
	if c.SecurityContext.Privileged != nil {
		t.Errorf("Privileged = %v, want nil", c.SecurityContext.Privileged)
	}
	if c.SecurityContext.Capabilities != nil {
		t.Errorf("Capabilities = %+v, want nil", c.SecurityContext.Capabilities)
	}
}

func TestPodmanEngine_AgentGetsDockerHost(t *testing.T) {
	pod, err := RenderPod(Inputs{Env: baseEnv("podman-dockerhost"), Class: podmanClass(), ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == nil || *c.Name != AgentContainerName {
			continue
		}
		got := map[string]string{}
		for _, e := range c.Env {
			if e.Name != nil && e.Value != nil {
				got[*e.Name] = *e.Value
			}
			if e.ValueFrom != nil {
				t.Errorf("agent env %v has ValueFrom set", e.Name)
			}
		}
		if len(got) != 2 {
			t.Fatalf("agent Env = %v, want exactly DOCKER_HOST and CONTAINER_HOST", got)
		}
		if got["DOCKER_HOST"] != PodmanDockerHost {
			t.Errorf("DOCKER_HOST = %q, want %q", got["DOCKER_HOST"], PodmanDockerHost)
		}
		if got["CONTAINER_HOST"] != PodmanDockerHost {
			t.Errorf("CONTAINER_HOST = %q, want %q", got["CONTAINER_HOST"], PodmanDockerHost)
		}
		return
	}
	t.Fatal("agent container not found")
}

func TestPodmanEngine_IsANativeSidecarOrderedBeforeSandboxctl(t *testing.T) {
	pod, err := RenderPod(Inputs{Env: baseEnv("podman-order"), Class: podmanClass(), ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	ics := pod.Spec.InitContainers
	if len(ics) < 2 {
		t.Fatalf("expected at least 2 init containers, got %d", len(ics))
	}
	if ics[0].Name == nil || *ics[0].Name != PodmanContainerName {
		t.Errorf("InitContainers[0].Name = %v, want %q", ics[0].Name, PodmanContainerName)
	}
	if ics[0].RestartPolicy == nil || *ics[0].RestartPolicy != "Always" {
		t.Errorf("InitContainers[0].RestartPolicy = %v, want Always", ics[0].RestartPolicy)
	}
	if ics[1].Name == nil || *ics[1].Name != SidecarContainerName {
		t.Errorf("InitContainers[1].Name = %v, want %q", ics[1].Name, SidecarContainerName)
	}

	// With a Restore plan, the last init container is still "restore",
	// with no RestartPolicy (a plain, one-shot init container).
	pod2, err := RenderPod(Inputs{Env: baseEnv("podman-order-restore"), Class: podmanClass(), ClusterID: "test-cluster", SidecarImage: "test-sidecar:test", Restore: restorePlan()})
	if err != nil {
		t.Fatalf("RenderPod (restore): %v", err)
	}
	ics2 := pod2.Spec.InitContainers
	last := ics2[len(ics2)-1]
	if last.Name == nil || *last.Name != RestoreContainerName {
		t.Errorf("last init container = %v, want %q", last.Name, RestoreContainerName)
	}
	if last.RestartPolicy != nil {
		t.Errorf("restore container RestartPolicy = %v, want unset", *last.RestartPolicy)
	}
}

func TestPodmanEngine_GraphRootIsAnEmptyDirOutsideEverySnapshottedPath(t *testing.T) {
	pod, err := RenderPod(Inputs{Env: baseEnv("podman-graph"), Class: podmanClass(), ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	found := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == nil || *v.Name != podmanGraphVolumeName {
			continue
		}
		found = true
		if v.EmptyDir == nil {
			t.Error("podman-graph volume has no EmptyDir")
		}
		if v.PersistentVolumeClaim != nil || v.Secret != nil || v.Projected != nil || v.HostPath != nil {
			t.Errorf("podman-graph volume has a non-EmptyDir source: %+v", v)
		}
	}
	if !found {
		t.Fatal("podman-graph volume not found in rendered pod")
	}
	if strings.HasPrefix(PodmanGraphMountPath, WorkspaceMountPath) || PodmanGraphMountPath == WorkspaceMountPath {
		t.Errorf("PodmanGraphMountPath %q must not be under WorkspaceMountPath %q", PodmanGraphMountPath, WorkspaceMountPath)
	}
	if strings.HasPrefix(PodmanGraphMountPath, AgentHomePath) || PodmanGraphMountPath == AgentHomePath {
		t.Errorf("PodmanGraphMountPath %q must not be under AgentHomePath %q", PodmanGraphMountPath, AgentHomePath)
	}

	// No other container mounts it.
	for _, c := range pod.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name != nil && *m.Name == podmanGraphVolumeName {
				t.Errorf("container %v must not mount %q", c.Name, podmanGraphVolumeName)
			}
		}
	}
	for _, c := range pod.Spec.InitContainers {
		if c.Name != nil && *c.Name == PodmanContainerName {
			continue
		}
		for _, m := range c.VolumeMounts {
			if m.Name != nil && *m.Name == podmanGraphVolumeName {
				t.Errorf("init container %v must not mount %q", c.Name, podmanGraphVolumeName)
			}
		}
	}
}

func TestPodmanEngine_StorageDriverResolution(t *testing.T) {
	cases := []struct {
		driver string
		want   string
	}{
		{"", "overlay"},
		{"auto", "overlay"},
		{"overlay", "overlay"},
		{"vfs", "vfs"},
	}
	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			class := podmanClass()
			class.Spec.Engine.StorageDriver = tc.driver
			pod, err := RenderPod(Inputs{Env: baseEnv("podman-driver"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
			if err != nil {
				t.Fatalf("RenderPod: %v", err)
			}
			c := podmanContainerOf(t, pod)
			if len(c.Args) != 1 {
				t.Fatalf("podman container Args = %v, want exactly one bootstrap script", c.Args)
			}
			script := c.Args[0]
			if !strings.Contains(script, `driver = "`+tc.want+`"`) {
				t.Errorf("script does not contain driver = %q:\n%s", tc.want, script)
			}
			if strings.Contains(script, "mount_program") {
				t.Errorf("script must never emit mount_program:\n%s", script)
			}
		})
	}
}

func TestPodmanEngine_RegistriesConf(t *testing.T) {
	cases := []struct {
		name      string
		mirror    *v1alpha1.RegistryMirrorService
		wantNoReg bool
		wantHost  string
		wantInsec bool
		wantPairs int
	}{
		{name: "no mirror", mirror: nil, wantNoReg: true},
		{
			name:      "http mirror",
			mirror:    &v1alpha1.RegistryMirrorService{URL: "http://registry-cache.ns.svc.cluster.local:5000"},
			wantHost:  "registry-cache.ns.svc.cluster.local:5000",
			wantInsec: true,
			wantPairs: 1,
		},
		{
			name:      "https mirror",
			mirror:    &v1alpha1.RegistryMirrorService{URL: "https://harbor.example.com"},
			wantHost:  "harbor.example.com",
			wantInsec: false,
			wantPairs: 1,
		},
		{
			name:      "path-bearing url",
			mirror:    &v1alpha1.RegistryMirrorService{URL: "https://harbor.example.com/dockerhub"},
			wantHost:  "harbor.example.com/dockerhub",
			wantInsec: false,
			wantPairs: 1,
		},
		{
			name: "multiple registries, sorted",
			mirror: &v1alpha1.RegistryMirrorService{
				URL:        "http://registry-cache.ns.svc.cluster.local:5000",
				Registries: []string{"quay.io", "docker.io", "docker.io"},
			},
			wantHost:  "registry-cache.ns.svc.cluster.local:5000",
			wantInsec: true,
			wantPairs: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := withEngine(minimalClass(), v1alpha1.EngineTypeRootlessPodman)
			class.Spec.Services.RegistryMirror = tc.mirror
			pod, err := RenderPod(Inputs{Env: baseEnv("podman-registries"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
			if err != nil {
				t.Fatalf("RenderPod: %v", err)
			}
			script := podmanContainerOf(t, pod).Args[0]
			if tc.wantNoReg {
				if strings.Contains(script, "[[registry]]") {
					t.Errorf("script should not contain [[registry]] with no mirror:\n%s", script)
				}
				return
			}
			gotPairs := strings.Count(script, "[[registry]]")
			if gotPairs != tc.wantPairs {
				t.Errorf("got %d [[registry]] blocks, want %d:\n%s", gotPairs, tc.wantPairs, script)
			}
			if !strings.Contains(script, `location = "`+tc.wantHost+`"`) {
				t.Errorf("script missing mirror location %q:\n%s", tc.wantHost, script)
			}
			if tc.wantInsec != strings.Contains(script, "insecure = true") {
				t.Errorf("script insecure=true presence = %v, want %v:\n%s", strings.Contains(script, "insecure = true"), tc.wantInsec, script)
			}
		})
	}
}

func TestPodmanEngine_ImageDefaultsToPinnedDigest(t *testing.T) {
	pod, err := RenderPod(Inputs{Env: baseEnv("podman-image-default"), Class: podmanClass(), ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	c := podmanContainerOf(t, pod)
	if c.Image == nil || *c.Image != DefaultPodmanImage {
		t.Errorf("podman container Image = %v, want %q", c.Image, DefaultPodmanImage)
	}
	if !strings.Contains(DefaultPodmanImage, "@sha256:") {
		t.Errorf("DefaultPodmanImage %q does not contain @sha256:", DefaultPodmanImage)
	}
	if !strings.HasPrefix(DefaultPodmanImage, "quay.io/podman/stable") {
		t.Errorf("DefaultPodmanImage %q does not start with quay.io/podman/stable", DefaultPodmanImage)
	}
}

func TestPodmanEngine_RejectsUnpinnedImageOverride(t *testing.T) {
	class := podmanClass()
	class.Spec.Engine.Image = "quay.io/podman/stable:v5.8.2"
	_, err := RenderPod(Inputs{Env: baseEnv("podman-unpinned"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"})
	if err == nil {
		t.Fatal("RenderPod with an unpinned image override: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "is not pinned by digest") {
		t.Errorf("error = %q, want substring \"is not pinned by digest\"", err.Error())
	}

	class2 := podmanClass()
	class2.Spec.Engine.Image = "quay.io/podman/stable@sha256:abc123"
	if _, err := RenderPod(Inputs{Env: baseEnv("podman-pinned"), Class: class2, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}); err != nil {
		t.Errorf("RenderPod with a digest-pinned image override: %v, want nil", err)
	}
}

func TestRenderPod_PodmanMinimal(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeRootlessPodman)
	in := Inputs{Env: baseEnv("podman-minimal"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test"}
	assertGoldenPod(t, "podman-minimal", in)
}

func TestRenderPod_PodmanFull(t *testing.T) {
	class := fullClass()
	class.Spec.Engine.Type = v1alpha1.EngineTypeRootlessPodman
	class.Spec.Services.RegistryMirror = &v1alpha1.RegistryMirrorService{
		URL:        "http://registry-cache.ai-sandbox-e2e.svc.cluster.local:5000",
		Registries: []string{"docker.io", "quay.io"},
	}
	in := Inputs{
		Env:          baseEnv("podman-full", withPrompt("do the full podman thing")),
		Class:        class,
		Credentials:  Credentials{GitProxyToken: "fake-git-proxy-token-podman-full"}, //nolint:gosec // G101: deliberately fake test fixture value
		ClusterID:    "test-cluster",
		SidecarImage: "test-sidecar:test",
	}
	assertGoldenPod(t, "podman-full", in)
}

func TestRenderPod_PodmanRestore(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeRootlessPodman)
	in := Inputs{Env: baseEnv("podman-restore"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test", Restore: restorePlan()}
	assertGoldenPod(t, "podman-restore", in)
}
