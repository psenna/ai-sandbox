package apitest

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// validClass returns a SandboxClass with every field populated, including
// non-default values for pointer/optional fields, so a round-trip test can
// catch any field silently dropped by the schema.
func validClass(name string) *sandboxv1alpha1.SandboxClass {
	return &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{
				Image: "ghcr.io/psenna/ai-sandbox-agent:v1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
				Model: &sandboxv1alpha1.ModelSpec{
					Default: "claude-sonnet-4-5",
					Haiku:   "claude-haiku-4-5",
					Sonnet:  "claude-sonnet-4-5",
					Opus:    "claude-opus-4-5",
				},
			},
			Engine: sandboxv1alpha1.EngineSpec{
				Type:          sandboxv1alpha1.EngineTypeRootlessPodman,
				Image:         "quay.io/podman/stable:v5",
				StorageDriver: "overlay",
			},
			Services: sandboxv1alpha1.ServicesSpec{
				GitProxy: &sandboxv1alpha1.GitProxyService{
					GitURL:    "https://git-proxy.example.com",
					BrokerURL: "https://git-proxy.example.com/broker",
					TokenSecretRef: sandboxv1alpha1.SecretKeyRef{
						Name: "git-proxy-token",
						Key:  "token",
					},
				},
				DependaProxy: &sandboxv1alpha1.DependaProxyService{
					NpmURL:     "http://dependaproxy.example.com/npm",
					PypiURL:    "http://dependaproxy.example.com/pypi",
					GoproxyURL: "http://dependaproxy.example.com/goproxy",
				},
				Ollama: &sandboxv1alpha1.OllamaService{
					BaseURL: "http://ollama.example.com:11434",
				},
			},
			Storage: sandboxv1alpha1.StorageSpec{
				Workspace: sandboxv1alpha1.WorkspaceSpec{
					Size:             "50Gi",
					StorageClassName: ptr.To("fast-ssd"),
				},
				Backend: sandboxv1alpha1.BackendSpec{
					Type: sandboxv1alpha1.StorageBackendTypeS3,
					S3: &sandboxv1alpha1.S3Backend{
						Endpoint: "https://s3.example.com",
						Bucket:   "sandbox-snapshots",
						Region:   "us-east-1",
						Prefix:   "prod/",
						CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{
							Name: "s3-creds",
							Key:  "token",
						},
						ForcePathStyle: ptr.To(false),
					},
				},
				WarmCacheTTL: "45m",
			},
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation: sandboxv1alpha1.NetworkIsolationRestricted,
				ExtraEgress: []sandboxv1alpha1.EgressPeer{
					{CIDR: "1.2.3.4/32"},
					{Selector: &sandboxv1alpha1.PeerSelector{Namespace: "ai-sandbox"}},
				},
			},
			Timeouts: sandboxv1alpha1.TimeoutsSpec{
				Running: "3h",
				Waiting: "12h",
				Total:   "48h",
			},
		},
	}
}

func TestClassRoundTrip(t *testing.T) {
	want := validClass("roundtrip-class")

	if err := k8s.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := &sandboxv1alpha1.SandboxClass{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: want.Name}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	assertAgentRoundTrip(t, got.Spec.Agent, want.Spec.Agent)
	if got.Spec.Engine != want.Spec.Engine {
		t.Errorf("Engine = %+v, want %+v", got.Spec.Engine, want.Spec.Engine)
	}
	assertServicesRoundTrip(t, got.Spec.Services, want.Spec.Services)
	assertStorageRoundTrip(t, got.Spec.Storage, want.Spec.Storage)

	if got.Spec.Network.Isolation != want.Spec.Network.Isolation {
		t.Errorf("Network.Isolation = %q, want %q", got.Spec.Network.Isolation, want.Spec.Network.Isolation)
	}
	if !reflect.DeepEqual(got.Spec.Network.ExtraEgress, want.Spec.Network.ExtraEgress) {
		t.Errorf("Network.ExtraEgress = %v, want %v", got.Spec.Network.ExtraEgress, want.Spec.Network.ExtraEgress)
	}
	if got.Spec.Timeouts != want.Spec.Timeouts {
		t.Errorf("Timeouts = %+v, want %+v", got.Spec.Timeouts, want.Spec.Timeouts)
	}
}

func assertAgentRoundTrip(t *testing.T, got, want sandboxv1alpha1.AgentSpec) {
	t.Helper()
	if got.Image != want.Image {
		t.Errorf("Agent.Image = %q, want %q", got.Image, want.Image)
	}
	if !got.Resources.Requests.Cpu().Equal(*want.Resources.Requests.Cpu()) {
		t.Errorf("Agent.Resources.Requests.Cpu() = %v, want %v", got.Resources.Requests.Cpu(), want.Resources.Requests.Cpu())
	}
	if !got.Resources.Requests.Memory().Equal(*want.Resources.Requests.Memory()) {
		t.Errorf("Agent.Resources.Requests.Memory() = %v, want %v", got.Resources.Requests.Memory(), want.Resources.Requests.Memory())
	}
	if !got.Resources.Limits.Cpu().Equal(*want.Resources.Limits.Cpu()) {
		t.Errorf("Agent.Resources.Limits.Cpu() = %v, want %v", got.Resources.Limits.Cpu(), want.Resources.Limits.Cpu())
	}
	if !got.Resources.Limits.Memory().Equal(*want.Resources.Limits.Memory()) {
		t.Errorf("Agent.Resources.Limits.Memory() = %v, want %v", got.Resources.Limits.Memory(), want.Resources.Limits.Memory())
	}
	if got.Model == nil || want.Model == nil || *got.Model != *want.Model {
		t.Errorf("Agent.Model = %+v, want %+v", got.Model, want.Model)
	}
}

func assertServicesRoundTrip(t *testing.T, got, want sandboxv1alpha1.ServicesSpec) {
	t.Helper()
	if got.GitProxy == nil || *got.GitProxy != *want.GitProxy {
		t.Errorf("Services.GitProxy = %+v, want %+v", got.GitProxy, want.GitProxy)
	}
	if got.DependaProxy == nil || *got.DependaProxy != *want.DependaProxy {
		t.Errorf("Services.DependaProxy = %+v, want %+v", got.DependaProxy, want.DependaProxy)
	}
	if got.Ollama == nil || *got.Ollama != *want.Ollama {
		t.Errorf("Services.Ollama = %+v, want %+v", got.Ollama, want.Ollama)
	}
}

func assertStorageRoundTrip(t *testing.T, got, want sandboxv1alpha1.StorageSpec) {
	t.Helper()
	if got.Workspace.Size != want.Workspace.Size {
		t.Errorf("Storage.Workspace.Size = %q, want %q", got.Workspace.Size, want.Workspace.Size)
	}
	if got.Workspace.StorageClassName == nil || want.Workspace.StorageClassName == nil ||
		*got.Workspace.StorageClassName != *want.Workspace.StorageClassName {
		t.Errorf("Storage.Workspace.StorageClassName = %v, want %v", derefStr(got.Workspace.StorageClassName), derefStr(want.Workspace.StorageClassName))
	}
	if got.WarmCacheTTL != want.WarmCacheTTL {
		t.Errorf("Storage.WarmCacheTTL = %q, want %q", got.WarmCacheTTL, want.WarmCacheTTL)
	}
	if got.Backend.Type != want.Backend.Type {
		t.Errorf("Storage.Backend.Type = %q, want %q", got.Backend.Type, want.Backend.Type)
	}

	gotS3, wantS3 := got.Backend.S3, want.Backend.S3
	if gotS3 == nil || wantS3 == nil {
		t.Fatalf("Storage.Backend.S3 = %v, want %v", gotS3, wantS3)
	}
	if gotS3.Endpoint != wantS3.Endpoint || gotS3.Bucket != wantS3.Bucket || gotS3.Region != wantS3.Region ||
		gotS3.Prefix != wantS3.Prefix || gotS3.CredentialsSecretRef != wantS3.CredentialsSecretRef {
		t.Errorf("Storage.Backend.S3 = %+v, want %+v", gotS3, wantS3)
	}
	if gotS3.ForcePathStyle == nil || wantS3.ForcePathStyle == nil || *gotS3.ForcePathStyle != *wantS3.ForcePathStyle {
		t.Errorf("Storage.Backend.S3.ForcePathStyle = %v, want %v", derefBool(gotS3.ForcePathStyle), derefBool(wantS3.ForcePathStyle))
	}
}

func TestClassDefaults(t *testing.T) {
	minimal := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "defaults-class"},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Storage: sandboxv1alpha1.StorageSpec{
				Backend: sandboxv1alpha1.BackendSpec{
					Type: sandboxv1alpha1.StorageBackendTypeS3,
					S3: &sandboxv1alpha1.S3Backend{
						Endpoint: "https://s3.example.com",
						Bucket:   "sandbox-snapshots",
						CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{
							Name: "s3-creds",
						},
					},
				},
			},
		},
	}

	if err := k8s.Create(ctx, minimal); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := &sandboxv1alpha1.SandboxClass{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: minimal.Name}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Spec.Engine.Type != sandboxv1alpha1.EngineTypeRootlessPodman {
		t.Errorf("Engine.Type = %q, want %q", got.Spec.Engine.Type, sandboxv1alpha1.EngineTypeRootlessPodman)
	}
	if got.Spec.Engine.StorageDriver != "auto" {
		t.Errorf("Engine.StorageDriver = %q, want %q", got.Spec.Engine.StorageDriver, "auto")
	}
	if got.Spec.Network.Isolation != sandboxv1alpha1.NetworkIsolationRestricted {
		t.Errorf("Network.Isolation = %q, want %q", got.Spec.Network.Isolation, sandboxv1alpha1.NetworkIsolationRestricted)
	}
	if got.Spec.Storage.WarmCacheTTL != "30m" {
		t.Errorf("Storage.WarmCacheTTL = %q, want %q", got.Spec.Storage.WarmCacheTTL, "30m")
	}
	if got.Spec.Storage.Workspace.Size != "20Gi" {
		t.Errorf("Storage.Workspace.Size = %q, want %q", got.Spec.Storage.Workspace.Size, "20Gi")
	}
	wantTimeouts := sandboxv1alpha1.TimeoutsSpec{Running: "6h", Waiting: "24h", Total: "72h"}
	if got.Spec.Timeouts != wantTimeouts {
		t.Errorf("Timeouts = %+v, want %+v", got.Spec.Timeouts, wantTimeouts)
	}
	if got.Spec.Storage.Backend.S3 == nil {
		t.Fatalf("Storage.Backend.S3 = nil")
	}
	if got.Spec.Storage.Backend.S3.CredentialsSecretRef.Key != "token" {
		t.Errorf("Storage.Backend.S3.CredentialsSecretRef.Key = %q, want %q", got.Spec.Storage.Backend.S3.CredentialsSecretRef.Key, "token")
	}
	if got.Spec.Storage.Backend.S3.ForcePathStyle == nil || !*got.Spec.Storage.Backend.S3.ForcePathStyle {
		t.Errorf("Storage.Backend.S3.ForcePathStyle = %v, want true", derefBool(got.Spec.Storage.Backend.S3.ForcePathStyle))
	}
}

func TestClassRejections(t *testing.T) {
	base := func() *sandboxv1alpha1.SandboxClass {
		c := validClass("")
		return c
	}

	cases := []struct {
		name   string
		mutate func(*sandboxv1alpha1.SandboxClass)
	}{
		{
			name: "bad-engine-type",
			mutate: func(c *sandboxv1alpha1.SandboxClass) {
				c.Spec.Engine.Type = "docker"
			},
		},
		{
			name: "bad-network-isolation",
			mutate: func(c *sandboxv1alpha1.SandboxClass) {
				c.Spec.Network.Isolation = "Sealed"
			},
		},
		{
			name: "bad-backend-type",
			mutate: func(c *sandboxv1alpha1.SandboxClass) {
				c.Spec.Storage.Backend.Type = "nfs"
			},
		},
		{
			name: "missing-agent-image",
			mutate: func(c *sandboxv1alpha1.SandboxClass) {
				c.Spec.Agent.Image = ""
			},
		},
		{
			name: "s3-type-without-s3-block",
			mutate: func(c *sandboxv1alpha1.SandboxClass) {
				c.Spec.Storage.Backend.Type = sandboxv1alpha1.StorageBackendTypeS3
				c.Spec.Storage.Backend.S3 = nil
			},
		},
		{
			name: "pvc-type-without-pvc-block",
			mutate: func(c *sandboxv1alpha1.SandboxClass) {
				c.Spec.Storage.Backend.Type = sandboxv1alpha1.StorageBackendTypePVC
				c.Spec.Storage.Backend.S3 = nil
			},
		},
		{
			name: "pvc-block-set-while-type-s3",
			mutate: func(c *sandboxv1alpha1.SandboxClass) {
				c.Spec.Storage.Backend.Type = sandboxv1alpha1.StorageBackendTypeS3
				c.Spec.Storage.Backend.PVC = &sandboxv1alpha1.PVCBackend{ClaimName: "workspace"}
			},
		},
		{
			name: "non-url-s3-endpoint",
			mutate: func(c *sandboxv1alpha1.SandboxClass) {
				c.Spec.Storage.Backend.S3.Endpoint = "not-a-url"
			},
		},
		{
			name: "invalid-duration",
			mutate: func(c *sandboxv1alpha1.SandboxClass) {
				c.Spec.Storage.WarmCacheTTL = "6 hours"
			},
		},
		{
			name: "extraegress-both-cidr-and-selector",
			mutate: func(c *sandboxv1alpha1.SandboxClass) {
				c.Spec.Network.ExtraEgress = []sandboxv1alpha1.EgressPeer{{
					CIDR:     "1.2.3.4/32",
					Selector: &sandboxv1alpha1.PeerSelector{Namespace: "ai-sandbox"},
				}}
			},
		},
		{
			name: "extraegress-neither-cidr-nor-selector",
			mutate: func(c *sandboxv1alpha1.SandboxClass) {
				c.Spec.Network.ExtraEgress = []sandboxv1alpha1.EgressPeer{{}}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := base()
			obj.Name = "reject-" + tc.name
			tc.mutate(obj)

			err := k8s.Create(ctx, obj)
			if err == nil {
				t.Fatalf("Create succeeded, want rejection")
			}
			if !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) {
				t.Errorf("error is neither Invalid nor BadRequest: %v", err)
			}
			t.Logf("server message: %v", err)
		})
	}
}

func derefStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func derefBool(b *bool) string {
	if b == nil {
		return "<nil>"
	}
	if *b {
		return "true"
	}
	return "false"
}
