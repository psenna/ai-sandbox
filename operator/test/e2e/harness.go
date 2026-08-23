package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// Config is the e2e suite's environment-derived configuration. Every field
// has an env var (E2E_*) with a sane default so `make e2e-run` works
// out-of-the-box after `make e2e-up`; see LoadConfig.
type Config struct {
	Host        string
	MinIOPort   int
	BrokerPort  int
	ModelPort   int
	S3ProxyPort int

	OperatorNamespace string
	ServicesNamespace string

	AgentImage     string
	SnapshotBucket string
	FixtureBucket  string
	BrokerToken    string

	ArtifactDir string

	PhaseTimeout time.Duration
	PodTimeout   time.Duration
	Poll         time.Duration
}

// LoadConfig reads Config from the environment, falling back to defaults
// that match test/e2e/manifests (NodePorts, namespaces, image tags) and
// hack/e2e-up.sh.
func LoadConfig() Config {
	return Config{
		Host:              envOr("E2E_HOST", "127.0.0.1"),
		MinIOPort:         envOrInt("E2E_MINIO_PORT", 30900),
		BrokerPort:        envOrInt("E2E_BROKER_PORT", 30901),
		ModelPort:         envOrInt("E2E_MODEL_PORT", 30902),
		S3ProxyPort:       envOrInt("E2E_S3PROXY_PORT", 30903),
		OperatorNamespace: envOr("E2E_OPERATOR_NS", "ai-sandbox-operator-system"),
		ServicesNamespace: envOr("E2E_SERVICES_NS", "ai-sandbox-e2e"),
		AgentImage:        envOr("E2E_AGENT_IMAGE", "ai-sandbox-e2e-agent:test"),
		SnapshotBucket:    envOr("E2E_BUCKET", "sandbox-snapshots"),
		FixtureBucket:     envOr("E2E_FIXTURE_BUCKET", "e2e-fixtures"),
		BrokerToken:       envOr("E2E_BROKER_TOKEN", "e2e-token"),
		ArtifactDir:       envOr("E2E_ARTIFACT_DIR", "./_artifacts"),
		PhaseTimeout:      envOrDuration("E2E_PHASE_TIMEOUT", 5*time.Minute),
		PodTimeout:        envOrDuration("E2E_POD_TIMEOUT", 3*time.Minute),
		Poll:              envOrDuration("E2E_POLL", 2*time.Second),
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envOrInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envOrDuration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// Harness bundles everything a spec needs to drive the cluster: config, a
// controller-runtime client (scheme-registered for core/rbac/batch/apps/
// networking plus the sandbox CRDs) and a raw clientset (for pod logs and
// exec, neither of which the controller-runtime client exposes).
type Harness struct {
	Cfg        Config
	RestConfig *rest.Config
	Client     client.Client
	Clientset  *kubernetes.Clientset
}

// New builds a Harness from KUBECONFIG (or in-cluster config if unset).
// Fails the current spec via Gomega's global fail handler on any error --
// callers are expected to call this only from within a Ginkgo node
// (SynchronizedBeforeSuite, BeforeEach, etc).
func New(ctx context.Context) *Harness {
	restCfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "loading kubeconfig")

	sch := runtime.NewScheme()
	gomega.Expect(clientgoscheme.AddToScheme(sch)).To(gomega.Succeed())
	gomega.Expect(sandboxv1alpha1.AddToScheme(sch)).To(gomega.Succeed())

	c, err := client.New(restCfg, client.Options{Scheme: sch})
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "building controller-runtime client")

	cs, err := kubernetes.NewForConfig(restCfg)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "building clientset")

	_ = ctx
	return &Harness{Cfg: LoadConfig(), RestConfig: restCfg, Client: c, Clientset: cs}
}

// WaitForPlatformReady waits for the operator, MinIO and the platform
// doubles Deployments to each report at least one ready replica.
func (h *Harness) WaitForPlatformReady(ctx context.Context, timeout time.Duration) {
	h.waitDeploymentReady(ctx, h.Cfg.OperatorNamespace, "ai-sandbox-operator-controller-manager", timeout)
	h.waitDeploymentReady(ctx, h.Cfg.ServicesNamespace, "minio", timeout)
	h.waitDeploymentReady(ctx, h.Cfg.ServicesNamespace, "platform-doubles", timeout)
}

func (h *Harness) waitDeploymentReady(ctx context.Context, ns, name string, timeout time.Duration) {
	gomega.EventuallyWithOffset(1, func() int32 {
		var d appsv1.Deployment
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &d); err != nil {
			return 0
		}
		return d.Status.ReadyReplicas
	}, timeout, h.Cfg.Poll).Should(gomega.BeNumerically(">=", 1), "deployment %s/%s never became ready", ns, name)
}

// NewNamespace creates a Namespace named "<prefix>-<8 hex>" and registers a
// DeferCleanup to delete it.
func (h *Harness) NewNamespace(ctx context.Context, prefix string) string {
	name := prefix + "-" + randHex(4)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	gomega.ExpectWithOffset(1, h.Client.Create(ctx, ns)).To(gomega.Succeed(), "creating namespace %s", name)
	ginkgo.DeferCleanup(func(ctx context.Context) {
		_ = h.Client.Delete(ctx, ns)
	})
	return name
}

func randHex(n int) string {
	b := make([]byte, n)
	_, err := rand.Read(b)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return hex.EncodeToString(b)
}

// ClassOption mutates a SandboxClass under construction; see CreateClass.
type ClassOption func(*sandboxv1alpha1.SandboxClass)

func WithEngine(t sandboxv1alpha1.EngineType) ClassOption {
	return func(c *sandboxv1alpha1.SandboxClass) { c.Spec.Engine.Type = t }
}

// WithNetworkIsolation sets the class's network isolation posture (#31):
// Restricted (a NetworkPolicy default-denies egress except for the resolved
// peers) or Open (no policy, unrestricted egress).
func WithNetworkIsolation(isolation sandboxv1alpha1.NetworkIsolation) ClassOption {
	return func(c *sandboxv1alpha1.SandboxClass) { c.Spec.Network.Isolation = isolation }
}

func WithAgentImage(image string) ClassOption {
	return func(c *sandboxv1alpha1.SandboxClass) { c.Spec.Agent.Image = image }
}

func WithAgentResources(r corev1.ResourceRequirements) ClassOption {
	return func(c *sandboxv1alpha1.SandboxClass) { c.Spec.Agent.Resources = r }
}

func WithS3Backend(endpoint, bucket, secretName string) ClassOption {
	return func(c *sandboxv1alpha1.SandboxClass) {
		c.Spec.Storage.Backend = sandboxv1alpha1.BackendSpec{
			Type: sandboxv1alpha1.StorageBackendTypeS3,
			S3: &sandboxv1alpha1.S3Backend{
				Endpoint:             endpoint,
				Bucket:               bucket,
				CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{Name: secretName, Key: "token"},
			},
		}
	}
}

// WithWarmCacheTTL sets the class's warm-cache TTL (storage.warmCacheTTL,
// a Go duration string like "30m"). It is the policy knob the warm-cache
// GC (#29) reclaims a frozen environment's workspace PVC on.
func WithWarmCacheTTL(d string) ClassOption {
	return func(c *sandboxv1alpha1.SandboxClass) { c.Spec.Storage.WarmCacheTTL = d }
}

// WithS3Endpoint overrides the class's default in-cluster MinIO endpoint
// (from CreateClass) with a different one -- used by freeze_test.go to
// route S3 traffic through the s3proxy fault-injection double instead
// (S3ProxyInClusterEndpoint) while leaving the bucket and credentials
// Secret reference untouched.
func WithS3Endpoint(endpoint string) ClassOption {
	return func(c *sandboxv1alpha1.SandboxClass) {
		if c.Spec.Storage.Backend.S3 != nil {
			c.Spec.Storage.Backend.S3.Endpoint = endpoint
		}
	}
}

func WithGitProxy(gitURL, brokerURL, secretName, secretKey string) ClassOption {
	return func(c *sandboxv1alpha1.SandboxClass) {
		c.Spec.Services.GitProxy = &sandboxv1alpha1.GitProxyService{
			GitURL:    gitURL,
			BrokerURL: brokerURL,
			TokenSecretRef: sandboxv1alpha1.SecretKeyRef{
				Name: secretName,
				Key:  secretKey,
			},
		}
	}
}

// WithPodmanEngine selects the rootless-podman engine AND declares the
// in-cluster pull-through registry cache (RegistryCacheInClusterURL) in one
// option, because the two are inseparable under this suite's default
// Restricted isolation (#31): a Restricted NetworkPolicy allows egress only
// to the class's declared peers, so without the mirror the podman sidecar
// can reach no registry at all (guard G22's chart-side counterpart to this
// same fact). ClassOption has no Harness in scope (see the type's own doc
// comment), so the registry-cache URL is computed the same way
// RegistryCacheInClusterURL does -- E2E_SERVICES_NS with LoadConfig's own
// default -- rather than duplicating a Harness dependency into the type.
func WithPodmanEngine() ClassOption {
	return func(c *sandboxv1alpha1.SandboxClass) {
		c.Spec.Engine.Type = sandboxv1alpha1.EngineTypeRootlessPodman
		c.Spec.Services.RegistryMirror = &sandboxv1alpha1.RegistryMirrorService{
			URL: fmt.Sprintf("http://registry-cache.%s.svc.cluster.local:5000", envOr("E2E_SERVICES_NS", "ai-sandbox-e2e")),
		}
	}
}

// WithRegistryMirror declares a pull-through registry-cache peer
// (spec.services.registryMirror), the network route the podman sidecar
// pulls workload images through under Restricted isolation.
func WithRegistryMirror(url string, registries ...string) ClassOption {
	return func(c *sandboxv1alpha1.SandboxClass) {
		c.Spec.Services.RegistryMirror = &sandboxv1alpha1.RegistryMirrorService{
			URL:        url,
			Registries: registries,
		}
	}
}

// WithDependaProxyNpm points the class's dependaProxy npm endpoint at url
// (the e2e npm-registry double, NpmRegistryInClusterURL), so
// NPM_CONFIG_REGISTRY reaches the agent (and, passed through by a workload
// container's own env, npm inside it) and the Restricted NetworkPolicy
// gains the corresponding egress peer.
func WithDependaProxyNpm(url string) ClassOption {
	return func(c *sandboxv1alpha1.SandboxClass) {
		c.Spec.Services.DependaProxy = &sandboxv1alpha1.DependaProxyService{NpmURL: url}
	}
}

// CreateClass creates a cluster-scoped SandboxClass with sane e2e defaults
// (engine=none, the e2e agent image, small resources, a 128Mi workspace,
// in-cluster MinIO as the S3 backend) and registers a DeferCleanup to
// delete it.
func (h *Harness) CreateClass(ctx context.Context, opts ...ClassOption) *sandboxv1alpha1.SandboxClass {
	minioEndpoint := fmt.Sprintf("http://minio.%s.svc.cluster.local:9000", h.Cfg.ServicesNamespace)

	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-class-"},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{
				Image: h.Cfg.AgentImage,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			},
			Engine: sandboxv1alpha1.EngineSpec{Type: sandboxv1alpha1.EngineTypeNone},
			Storage: sandboxv1alpha1.StorageSpec{
				Workspace: sandboxv1alpha1.WorkspaceSpec{Size: "128Mi"},
				Backend: sandboxv1alpha1.BackendSpec{
					Type: sandboxv1alpha1.StorageBackendTypeS3,
					S3: &sandboxv1alpha1.S3Backend{
						Endpoint:             minioEndpoint,
						Bucket:               h.Cfg.SnapshotBucket,
						CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{Name: "minio-creds", Key: "token"},
					},
				},
			},
		},
	}
	for _, opt := range opts {
		opt(class)
	}

	gomega.ExpectWithOffset(1, h.Client.Create(ctx, class)).To(gomega.Succeed(), "creating SandboxClass")
	ginkgo.DeferCleanup(func(ctx context.Context) {
		_ = h.Client.Delete(ctx, class)
	})
	return class
}

// EnvOption mutates a SandboxEnvironment under construction; see
// CreateEnvironment.
type EnvOption func(*sandboxv1alpha1.SandboxEnvironment)

func WithPrompt(prompt string) EnvOption {
	return func(e *sandboxv1alpha1.SandboxEnvironment) { e.Spec.Task.Prompt = prompt }
}

// WithScript joins directives with "\n" to become the task prompt the fake
// `claude` CLI (test/e2e/agent/claude) interprets one line at a time.
func WithScript(directives ...string) EnvOption {
	return WithPrompt(strings.Join(directives, "\n"))
}

func WithPriority(p int32) EnvOption {
	return func(e *sandboxv1alpha1.SandboxEnvironment) { e.Spec.Priority = p }
}

func WithSuspend(b bool) EnvOption {
	return func(e *sandboxv1alpha1.SandboxEnvironment) { e.Spec.Suspend = b }
}

// CreateEnvironment creates a SandboxEnvironment in ns referencing
// className, defaulting to WithScript("SCRIPT:exit 0"), and registers a
// DeferCleanup to delete it.
func (h *Harness) CreateEnvironment(ctx context.Context, ns, className string, opts ...EnvOption) *sandboxv1alpha1.SandboxEnvironment {
	env := &sandboxv1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-env-",
			Namespace:    ns,
		},
		Spec: sandboxv1alpha1.SandboxEnvironmentSpec{
			ClassRef: sandboxv1alpha1.ClassRef{Name: className},
			Repo:     "psenna/e2e-fixture",
			Task:     sandboxv1alpha1.TaskSpec{Prompt: "SCRIPT:exit 0"},
		},
	}
	for _, opt := range opts {
		opt(env)
	}

	gomega.ExpectWithOffset(1, h.Client.Create(ctx, env)).To(gomega.Succeed(), "creating SandboxEnvironment")
	ginkgo.DeferCleanup(func(ctx context.Context) {
		_ = h.Client.Delete(ctx, env)
	})
	return env
}

// WaitForPhase polls key's SandboxEnvironment until its status.phase equals
// phase, or fails the spec after timeout.
func (h *Harness) WaitForPhase(ctx context.Context, key client.ObjectKey, phase sandboxv1alpha1.Phase, timeout time.Duration) {
	gomega.EventuallyWithOffset(1, func() sandboxv1alpha1.Phase {
		return h.GetEnv(ctx, key).Status.Phase
	}, timeout, h.Cfg.Poll).Should(gomega.Equal(phase), "environment %s never reached phase %s", key, phase)
}

// WaitForWakeCount polls key's SandboxEnvironment until status.wakeCount
// equals want, or fails the spec after timeout. WakeCount is monotonic (it
// only ever increments, on the Restoring->Running transition, and never
// resets -- see internal/lifecycle/next.go and apply.go) and CAS-protected
// (store.go's patchStatus uses MergeFromWithOptimisticLock), so it cannot
// match a stale pre-wake value. That monotonicity is what makes it a
// reliable "the wake actually happened" signal -- unlike phase, which
// WaitForPhase can race to a stale match immediately after ClearWaitFor
// (the env is still Waiting until the controller reconciles toward Ready).
func (h *Harness) WaitForWakeCount(ctx context.Context, key client.ObjectKey, want int32) {
	gomega.EventuallyWithOffset(1, func() int32 {
		return h.GetEnv(ctx, key).Status.WakeCount
	}, h.Cfg.PhaseTimeout, h.Cfg.Poll).Should(gomega.Equal(want), "environment %s wakeCount never reached %d", key, want)
}

// WaitForAnyPhase polls key's SandboxEnvironment until its phase is one of
// phases, returning the phase actually observed.
func (h *Harness) WaitForAnyPhase(ctx context.Context, key client.ObjectKey, timeout time.Duration, phases ...sandboxv1alpha1.Phase) sandboxv1alpha1.Phase {
	var got sandboxv1alpha1.Phase
	gomega.EventuallyWithOffset(1, func() bool {
		got = h.GetEnv(ctx, key).Status.Phase
		for _, p := range phases {
			if got == p {
				return true
			}
		}
		return false
	}, timeout, h.Cfg.Poll).Should(gomega.BeTrue(), "environment %s never reached any of %v (last observed: %s)", key, phases, got)
	return got
}

// WaitForCondition polls key's SandboxEnvironment until it carries a
// condition of type condType with the given status and reason.
func (h *Harness) WaitForCondition(ctx context.Context, key client.ObjectKey, condType string, status metav1.ConditionStatus, reason string, timeout time.Duration) {
	gomega.EventuallyWithOffset(1, func() bool {
		return conditionMatches(h.GetEnv(ctx, key).Status.Conditions, condType, status, reason)
	}, timeout, h.Cfg.Poll).Should(gomega.BeTrue(), "environment %s never got condition %s=%s (reason %s)", key, condType, status, reason)
}

func conditionMatches(conds []metav1.Condition, condType string, status metav1.ConditionStatus, reason string) bool {
	c := apimeta.FindStatusCondition(conds, condType)
	if c == nil {
		return false
	}
	if c.Status != status {
		return false
	}
	return reason == "" || c.Reason == reason
}

// conditionMessage returns env's condition of type condType's Message, or ""
// if env carries no such condition. Used by specs that assert on a
// condition's actual explanatory text (e.g. EngineSecurityRelaxed's
// relaxation list), not just its status/reason.
func conditionMessage(env *sandboxv1alpha1.SandboxEnvironment, condType string) string {
	c := apimeta.FindStatusCondition(env.Status.Conditions, condType)
	if c == nil {
		return ""
	}
	return c.Message
}

// WaitForAgentPodPhase polls the agent pod for key's environment until its
// PodPhase equals phase.
func (h *Harness) WaitForAgentPodPhase(ctx context.Context, key client.ObjectKey, phase corev1.PodPhase, timeout time.Duration) {
	gomega.EventuallyWithOffset(1, func() corev1.PodPhase {
		pod := h.getAgentPodOrNil(ctx, key)
		if pod == nil {
			return ""
		}
		return pod.Status.Phase
	}, timeout, h.Cfg.Poll).Should(gomega.Equal(phase), "agent pod for %s never reached phase %s", key, phase)
}

// WaitForPVCBound polls the PVC named key.Name until its phase is Bound.
func (h *Harness) WaitForPVCBound(ctx context.Context, key client.ObjectKey, timeout time.Duration) {
	gomega.EventuallyWithOffset(1, func() corev1.PersistentVolumeClaimPhase {
		var pvc corev1.PersistentVolumeClaim
		if err := h.Client.Get(ctx, key, &pvc); err != nil {
			return ""
		}
		return pvc.Status.Phase
	}, timeout, h.Cfg.Poll).Should(gomega.Equal(corev1.ClaimBound), "PVC %s never bound", key)
}

// WaitForPVCGone polls the PVC named key.Name until it no longer exists
// (NotFound). This is the REAL cluster's deletion signal -- unlike envtest,
// kind runs kube-controller-manager, so the kubernetes.io/pvc-protection
// finalizer is removed and the object actually disappears once the warm-
// cache GC (#29) deletes it.
func (h *Harness) WaitForPVCGone(ctx context.Context, key client.ObjectKey, timeout time.Duration) {
	gomega.EventuallyWithOffset(1, func() bool {
		var pvc corev1.PersistentVolumeClaim
		err := h.Client.Get(ctx, key, &pvc)
		return apierrors.IsNotFound(err)
	}, timeout, h.Cfg.Poll).Should(gomega.BeTrue(), "PVC %s was never reclaimed", key)
}

// ListEnvPVCs returns the workspace PVCs (render.ChildNames(envName).PVC,
// always exactly zero or one object) for envName in ns. The workspace PVC is
// the only persistent volume a sandbox has -- the agent home is an emptyDir
// -- so an empty result is the assertion that the warm-cache GC reclaimed
// the environment's cache and none leaked behind it.
func (h *Harness) ListEnvPVCs(ctx context.Context, ns, envName string) []corev1.PersistentVolumeClaim {
	pvcName := render.ChildNames(envName).PVC
	var list corev1.PersistentVolumeClaimList
	gomega.ExpectWithOffset(1, h.Client.List(ctx, &list, client.InNamespace(ns))).To(gomega.Succeed(), "listing PVCs in %s", ns)
	var out []corev1.PersistentVolumeClaim
	for _, p := range list.Items {
		if p.Name == pvcName {
			out = append(out, p)
		}
	}
	return out
}

// WaitForRestoreAttempt polls key's SandboxEnvironment until
// status.restoreAttempt reports phase, returning the attempt status at that
// point. restoreAttempt is written ONLY by the restore init container
// (#29), so reaching a given phase is the direct, non-inferred observation
// of a wake progressing (or failing).
func (h *Harness) WaitForRestoreAttempt(ctx context.Context, key client.ObjectKey, phase sandboxv1alpha1.RestoreAttemptPhase, timeout time.Duration) *sandboxv1alpha1.RestoreAttemptStatus {
	var attempt *sandboxv1alpha1.RestoreAttemptStatus
	gomega.EventuallyWithOffset(1, func() sandboxv1alpha1.RestoreAttemptPhase {
		attempt = h.GetEnv(ctx, key).Status.RestoreAttempt
		if attempt == nil {
			return ""
		}
		return attempt.Phase
	}, timeout, h.Cfg.Poll).Should(gomega.Equal(phase), "environment %s restoreAttempt never reached phase %s", key, phase)
	return attempt
}

// ClearWaitFor clears status.waitFor on key's SandboxEnvironment -- the wake
// trigger. A frozen environment holding on status.waitFor stays in Waiting
// (the operator's own probe evaluation is #30); clearing the field unblocks
// the scheduler to grant a slot and recreate the pod, whose restore init
// container performs the actual wake.
func (h *Harness) ClearWaitFor(ctx context.Context, key client.ObjectKey) {
	first := h.GetEnv(ctx, key)
	modified := first.DeepCopy()
	modified.Status.WaitFor = nil
	gomega.ExpectWithOffset(1, h.Client.Status().Patch(ctx, modified, client.MergeFrom(first))).To(gomega.Succeed(), "clearing status.waitFor on %s", key)
}

// ExpectCondition asserts (once, no polling) that key's SandboxEnvironment
// currently carries a condition of type condType with the given status and
// reason.
func (h *Harness) ExpectCondition(ctx context.Context, key client.ObjectKey, condType string, status metav1.ConditionStatus, reason string) {
	conds := h.GetEnv(ctx, key).Status.Conditions
	gomega.ExpectWithOffset(1, conditionMatches(conds, condType, status, reason)).To(gomega.BeTrue(),
		"expected condition %s=%s (reason %s), got %+v", condType, status, reason, conds)
}

// ExpectStablePhase asserts key's SandboxEnvironment stays at phase for the
// full duration d (no flapping).
func (h *Harness) ExpectStablePhase(ctx context.Context, key client.ObjectKey, phase sandboxv1alpha1.Phase, d time.Duration) {
	gomega.ConsistentlyWithOffset(1, func() sandboxv1alpha1.Phase {
		return h.GetEnv(ctx, key).Status.Phase
	}, d, h.Cfg.Poll).Should(gomega.Equal(phase), "environment %s did not stay at phase %s", key, phase)
}

// ExpectAgentExitCode asserts the agent container in key's agent pod
// terminated with exactly code.
//
// The environment can reach Done -- and the controller can issue its
// success-path pod cleanup -- as soon as the sidecar records the agent's
// /v1/done outcome. That observation lives in the controller's informer
// cache, which can run ahead of this client's own cache reflecting the
// agent container's own Terminated state in the pod object we read. A
// single Get therefore races two caches and flakes on "agent is not
// terminated" whenever Done lands first (the woken agent calls sandbox-done
// then falls through to `exit 0` microseconds later, so the container IS
// terminating -- the test just has to wait for its cache to catch up). Poll
// for the terminated state instead. The success pod is deleted on Done but
// lingers in Terminating long enough to observe the exit code; if a poll
// lands after garbage collection, surface that as a legible failure rather
// than a nil-pointer panic.
func (h *Harness) ExpectAgentExitCode(ctx context.Context, key client.ObjectKey, code int32) {
	gomega.EventuallyWithOffset(1, func(g gomega.Gomega) {
		pod := h.getAgentPodOrNil(ctx, key)
		if pod == nil {
			g.Expect(pod).NotTo(gomega.BeNil(), "agent pod for %s vanished before its exit code could be read", key)
			return
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != render.AgentContainerName {
				continue
			}
			g.Expect(cs.State.Terminated).NotTo(gomega.BeNil(), "agent container %s/%s is not terminated", pod.Namespace, pod.Name)
			g.Expect(cs.State.Terminated.ExitCode).To(gomega.Equal(code), "agent container %s/%s exit code", pod.Namespace, pod.Name)
			return
		}
		ginkgo.Fail(fmt.Sprintf("pod %s/%s has no %q container status", pod.Namespace, pod.Name, render.AgentContainerName))
	}, h.Cfg.PodTimeout, h.Cfg.Poll).Should(gomega.Succeed())
}

// GetEnv fetches key's SandboxEnvironment, failing the spec if it cannot be
// read.
func (h *Harness) GetEnv(ctx context.Context, key client.ObjectKey) *sandboxv1alpha1.SandboxEnvironment {
	var env sandboxv1alpha1.SandboxEnvironment
	gomega.ExpectWithOffset(1, h.Client.Get(ctx, key, &env)).To(gomega.Succeed(), "getting SandboxEnvironment %s", key)
	return &env
}

// GetAgentPod fetches the agent pod for key's environment (name =
// render.ChildNames(key.Name).Pod), failing the spec if it cannot be read.
func (h *Harness) GetAgentPod(ctx context.Context, key client.ObjectKey) *corev1.Pod {
	names := render.ChildNames(key.Name)
	var pod corev1.Pod
	gomega.ExpectWithOffset(1, h.Client.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: names.Pod}, &pod)).
		To(gomega.Succeed(), "getting agent pod for %s", key)
	return &pod
}

func (h *Harness) getAgentPodOrNil(ctx context.Context, key client.ObjectKey) *corev1.Pod {
	names := render.ChildNames(key.Name)
	var pod corev1.Pod
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: names.Pod}, &pod); err != nil {
		return nil
	}
	return &pod
}

// AgentLogs returns the agent container's current logs for key's
// environment.
func (h *Harness) AgentLogs(ctx context.Context, key client.ObjectKey) string {
	pod := h.GetAgentPod(ctx, key)
	return h.PodLogs(ctx, pod.Namespace, pod.Name, render.AgentContainerName, false)
}

// PodLogs returns container's logs for pod in ns, best-effort: on error, the
// returned string describes the error rather than the caller getting an
// error value (diagnostics helpers must never panic or fail the calling
// spec on a logs-fetch problem).
func (h *Harness) PodLogs(ctx context.Context, ns, pod, container string, previous bool) string {
	data, err := h.Clientset.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
	}).DoRaw(ctx)
	if err != nil {
		return fmt.Sprintf("<error fetching logs for %s/%s container %s (previous=%v): %v>", ns, pod, container, previous, err)
	}
	return string(data)
}

// Exec runs cmd inside container in pod (namespace ns) via a client-go
// remotecommand SPDY executor -- no kubectl binary needed.
func (h *Harness) Exec(ctx context.Context, ns, pod, container string, cmd ...string) (stdout, stderr string, err error) {
	req := h.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(ns).
		Name(pod).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: container,
		Command:   cmd,
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, clientgoscheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(h.RestConfig, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("building SPDY executor: %w", err)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdoutBuf,
		Stderr: &stderrBuf,
	})
	return stdoutBuf.String(), stderrBuf.String(), err
}
