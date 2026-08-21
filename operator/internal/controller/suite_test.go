// internal/controller's envtest-backed test suite. Mirrors
// internal/apitest/suite_test.go's TestMain pattern exactly (skip guard,
// explicit "default" namespace creation since envtest runs no
// kube-controller-manager).
package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

var (
	testEnv *envtest.Environment
	k8s     client.Client
	k8sCfg  *rest.Config
	ctx     = context.Background()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "skipping controller envtest suite: KUBEBUILDER_ASSETS not set")
		os.Exit(0)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	k8sCfg, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start: %v\n", err)
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = rbacv1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = authorizationv1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme) // #31: ensureResources applies/observes NetworkPolicies
	_ = sandboxv1alpha1.AddToScheme(scheme)

	k8s, err = client.New(k8sCfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	// envtest runs kube-apiserver without kube-controller-manager, so the
	// default/kube-system/etc. namespaces it normally bootstraps don't
	// exist. Fixtures in this suite use "default"; create it explicitly.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	if err := k8s.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		fmt.Fprintf(os.Stderr, "creating default namespace: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	code := m.Run()
	_ = testEnv.Stop()
	os.Exit(code)
}

// fakeClock is a mutex-guarded, manually advanced clock injected into
// Reconciler.Clock so timeout evaluation is deterministic and fast in tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// factsStore is a mutex-guarded map of injectable ClusterFacts, keyed by
// NamespacedName, that backs an ObserveFunc for tests that need to control
// exactly what the reconciler observes.
type factsStore struct {
	mu    sync.Mutex
	facts map[types.NamespacedName]lifecycle.ClusterFacts
}

func newFactsStore() *factsStore {
	return &factsStore{facts: make(map[types.NamespacedName]lifecycle.ClusterFacts)}
}

func (s *factsStore) set(key types.NamespacedName, f lifecycle.ClusterFacts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.facts[key] = f
}

func (s *factsStore) observe(_ context.Context, env *sandboxv1alpha1.SandboxEnvironment, _ *sandboxv1alpha1.SandboxClass) (lifecycle.ClusterFacts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	if f, ok := s.facts[key]; ok {
		return f, nil
	}
	return lifecycle.ClusterFacts{}, nil
}

func newReconciler(t *testing.T, clk *fakeClock, fs *factsStore) *Reconciler {
	t.Helper()
	r := &Reconciler{
		Client:               k8s,
		Clock:                clk.Now,
		Observe:              fs.observe,
		ClassSecretNamespace: "default",
		ClusterID:            "test",
		SidecarImage:         "ai-sandbox-operator:test",
	}
	return r
}

// newResourceReconciler builds a Reconciler wired to the REAL observer
// (observeCluster), for tests that exercise #19's child-resource rendering,
// application and observation end-to-end rather than injecting facts.
// observeCluster is unexported, so this helper lives inside package
// controller itself (an internal test file), matching newReconciler's own
// pattern of direct field construction rather than going through
// SetupWithManager.
func newResourceReconciler(t *testing.T, clk *fakeClock) *Reconciler {
	t.Helper()
	r := &Reconciler{
		Client:               k8s,
		Clock:                clk.Now,
		ClassSecretNamespace: "default",
		ClusterID:            "test",
		SidecarImage:         "ai-sandbox-operator:test",
		// Recorder is an eventCapture (eventcapture_test.go), not nil (#33):
		// secretleak_test.go's Event-half assertions were vacuous before this
		// -- a nil Recorder makes every Eventf call site a silent no-op, so
		// there was never anything for those assertions to actually find. A
		// test that wants to inspect what was captured type-asserts
		// r.Recorder.(*eventCapture).
		Recorder: newEventCapture(),
	}
	r.Observe = r.observeCluster
	return r
}

// mustCreateSourceSecret creates (or reuses) namespace ns, then creates a
// Secret named name in it with a single key/value pair. Used to simulate the
// class-referenced Secret D3 describes: a Secret in the operator's own
// namespace that the operator reads and projects into an environment-scoped
// Secret.
func mustCreateSourceSecret(t *testing.T, ns, name, key, value string) *corev1.Secret {
	t.Helper()
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := k8s.Create(ctx, nsObj); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating namespace %s: %v", ns, err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{key: []byte(value)},
	}
	if err := k8s.Create(ctx, secret); err != nil {
		t.Fatalf("creating source secret %s/%s: %v", ns, name, err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, secret)
	})
	return secret
}

// mustCreateS3CredsSecret creates (or reuses) namespace ns, then creates a
// Secret named name with FAKE, deliberately non-real S3 credentials, using
// internal/storage's fixed data keys (#28's resolveCredentials reads these,
// not CredentialsSecretRef.Key -- see storage/doc.go's gap G1). Every test
// class in this suite references a Secret named "s3-creds" in "default"
// (the reconciler's ClassSecretNamespace in these tests), so
// mustCreateClass/mustCreateClassWithEngine/mustCreateClassWithGitProxy all
// call this so resolveCredentials succeeds for their S3 backend.
func mustCreateS3CredsSecret(t *testing.T, ns, name string) *corev1.Secret { //nolint:unparam // ns is always "default" today; kept as a parameter for the same reason mustCreateSourceSecret takes one
	t.Helper()
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := k8s.Create(ctx, nsObj); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating namespace %s: %v", ns, err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data: map[string][]byte{ //nolint:gosec // G101: deliberately fake test fixture values, not real credentials
			storage.SecretKeyAccessKeyID:     []byte("fake-access-key-id"),
			storage.SecretKeySecretAccessKey: []byte("fake-secret-access-key"),
		},
	}
	if err := k8s.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating s3 creds secret %s/%s: %v", ns, name, err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, secret)
	})
	return secret
}

// mustCreateClass creates a minimal valid SandboxClass named "default" with
// default timeouts. SandboxClass is cluster-scoped, so name must be unique
// per test; callers that need more than one class should build their own.
func mustCreateClass(t *testing.T) *sandboxv1alpha1.SandboxClass {
	t.Helper()
	mustCreateS3CredsSecret(t, "default", "s3-creds")
	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
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
			// The S3 endpoint is external (no <svc>.<ns>.svc match), so under
			// the CRD's Restricted default resolveNetworkPeers requires an
			// extraEgress CIDR peer (#31) -- 0.0.0.0/0 keeps the fixture's
			// focus on resource creation, not network isolation.
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation:   sandboxv1alpha1.NetworkIsolationRestricted,
				ExtraEgress: []sandboxv1alpha1.EgressPeer{{CIDR: "0.0.0.0/0"}},
			},
		},
	}
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, class)
	})
	return class
}

// mustCreateClassWithEngine creates a minimal valid SandboxClass named
// "default" with spec.engine.type set to engineType.
func mustCreateClassWithEngine(t *testing.T, engineType sandboxv1alpha1.EngineType) *sandboxv1alpha1.SandboxClass {
	t.Helper()
	mustCreateS3CredsSecret(t, "default", "s3-creds")
	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Engine: sandboxv1alpha1.EngineSpec{
				Type: engineType,
			},
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
			// See mustCreateClass: the external S3 endpoint needs an
			// extraEgress CIDR peer under the Restricted default (#31).
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation:   sandboxv1alpha1.NetworkIsolationRestricted,
				ExtraEgress: []sandboxv1alpha1.EgressPeer{{CIDR: "0.0.0.0/0"}},
			},
		},
	}
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, class)
	})
	return class
}

// getPod fetches the Pod at key, failing the test if it does not exist.
func getPod(t *testing.T, key types.NamespacedName) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	if err := k8s.Get(ctx, key, pod); err != nil {
		t.Fatalf("Get pod %s: %v", key, err)
	}
	return pod
}

// mustSetPodStatus forces the Pod at key's status via a direct
// Status().Update, bypassing everything else -- envtest runs no kubelet, so
// nothing else ever advances a Pod's phase. mutate is applied to the
// current status fetched fresh from the server.
func mustSetPodStatus(t *testing.T, key types.NamespacedName, mutate func(*corev1.PodStatus)) {
	t.Helper()
	pod := &corev1.Pod{}
	if err := k8s.Get(ctx, key, pod); err != nil {
		t.Fatalf("Get pod %s before mustSetPodStatus: %v", key, err)
	}
	mutate(&pod.Status)
	if err := k8s.Status().Update(ctx, pod); err != nil {
		t.Fatalf("mustSetPodStatus Status().Update(%s): %v", key, err)
	}
}

// mustCreateClassWithGitProxy creates a minimal valid SandboxClass named
// "default" whose services.gitProxy.tokenSecretRef points at secretName/key
// in secretNamespace -- the class-referenced Secret D3 describes.
func mustCreateClassWithGitProxy(t *testing.T, secretNamespace, secretName, key string) *sandboxv1alpha1.SandboxClass {
	t.Helper()
	mustCreateS3CredsSecret(t, "default", "s3-creds")
	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
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
			// The S3 endpoint and the bare-hostname git-proxy URLs are all
			// external (no <svc>.<ns>.svc match), so under the CRD's
			// Restricted default resolveNetworkPeers requires an extraEgress
			// CIDR peer (#31) -- 0.0.0.0/0 keeps the fixture's focus on
			// resource creation, not network isolation.
			Network: sandboxv1alpha1.NetworkSpec{
				Isolation:   sandboxv1alpha1.NetworkIsolationRestricted,
				ExtraEgress: []sandboxv1alpha1.EgressPeer{{CIDR: "0.0.0.0/0"}},
			},
			Services: sandboxv1alpha1.ServicesSpec{
				GitProxy: &sandboxv1alpha1.GitProxyService{
					GitURL:    "http://git-proxy:8080",
					BrokerURL: "http://git-proxy:8090",
					TokenSecretRef: sandboxv1alpha1.SecretKeyRef{
						Name: secretName,
						Key:  key,
					},
				},
			},
		},
	}
	// secretNamespace is only meaningful when it equals the reconciler's
	// ClassSecretNamespace; callers are responsible for keeping the two in
	// sync (the tokenSecretRef itself carries no namespace -- see D3).
	_ = secretNamespace
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass: %v", err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, class)
	})
	return class
}

// mustCreateEnv creates a minimal valid SandboxEnvironment named name in the
// "default" namespace, referencing the "default" SandboxClass.
func mustCreateEnv(t *testing.T, name string) *sandboxv1alpha1.SandboxEnvironment {
	t.Helper()
	env := &sandboxv1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: sandboxv1alpha1.SandboxEnvironmentSpec{
			ClassRef: sandboxv1alpha1.ClassRef{Name: "default"},
			Repo:     "org/repo",
			Task:     sandboxv1alpha1.TaskSpec{Prompt: "do stuff"},
		},
	}
	if err := k8s.Create(ctx, env); err != nil {
		t.Fatalf("creating SandboxEnvironment: %v", err)
	}
	return env
}

// reconcileOnce calls r.Reconcile for key, failing the test on error, and
// returns the result.
func reconcileOnce(t *testing.T, r *Reconciler, key types.NamespacedName) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile(%s): %v", key, err)
	}
	return res
}

// ---- #20 slot scheduler test helpers ----

// mustCreateNamespace creates namespace name, tolerating AlreadyExists (the
// suite shares a single envtest apiserver across tests).
func mustCreateNamespace(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8s.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating namespace %s: %v", name, err)
	}
}

// mustCreateEnvIn creates a minimal valid SandboxEnvironment named name in
// namespace ns (created if needed) with the given priority, referencing the
// "default" SandboxClass.
func mustCreateEnvIn(t *testing.T, ns, name string, priority int32) *sandboxv1alpha1.SandboxEnvironment {
	t.Helper()
	mustCreateNamespace(t, ns)
	env := &sandboxv1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: sandboxv1alpha1.SandboxEnvironmentSpec{
			ClassRef: sandboxv1alpha1.ClassRef{Name: "default"},
			Repo:     "org/repo",
			Task:     sandboxv1alpha1.TaskSpec{Prompt: "do stuff"},
			Priority: priority,
		},
	}
	if err := k8s.Create(ctx, env); err != nil {
		t.Fatalf("creating SandboxEnvironment %s/%s: %v", ns, name, err)
	}
	return env
}

// mustSetPhase forces key's status (phase, slot grant, queuedSince) via a
// direct Status().Update, bypassing the reconciler entirely -- for setting
// up scheduler-test fixtures that don't need a full reconcile history. A
// zero queuedSince leaves status.queuedSince untouched.
func mustSetPhase(t *testing.T, key types.NamespacedName, phase sandboxv1alpha1.Phase, granted bool, queuedSince time.Time) {
	t.Helper()
	env := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, key, env); err != nil {
		t.Fatalf("Get(%s) before mustSetPhase: %v", key, err)
	}
	env.Status.Phase = phase
	env.Status.Slot.Granted = granted
	if !queuedSince.IsZero() {
		qs := metav1.NewTime(queuedSince)
		env.Status.QueuedSince = &qs
	}
	if err := k8s.Status().Update(ctx, env); err != nil {
		t.Fatalf("mustSetPhase Status().Update(%s): %v", key, err)
	}
}

// listEnvs lists every SandboxEnvironment in namespace ns ("" = all
// namespaces).
func listEnvs(t *testing.T, ns string) []sandboxv1alpha1.SandboxEnvironment {
	t.Helper()
	var list sandboxv1alpha1.SandboxEnvironmentList
	opts := []client.ListOption{}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	if err := k8s.List(ctx, &list, opts...); err != nil {
		t.Fatalf("listEnvs(%s): %v", ns, err)
	}
	return list.Items
}

// countGranted independently counts live environments in namespace ns
// ("" = all namespaces) with status.slot.granted == true. Deliberately
// hand-written rather than calling scheduler.Partition, so it's a genuine
// independent check on the code under test. Scoped to a namespace (a
// deviation from the plan's ()-arity sketch) because this suite's fixtures
// from other tests share the same envtest apiserver and are never cleaned
// up -- an unscoped count would be polluted by whatever other tests in this
// package happen to have run first.
func countGranted(t *testing.T, ns string) int {
	t.Helper()
	n := 0
	for _, e := range listEnvs(t, ns) {
		if e.Status.Slot.Granted {
			n++
		}
	}
	return n
}

// countOccupying independently counts live environments in namespace ns
// ("" = all namespaces) holding compute (Restoring/Running/Freezing phase,
// or granted). Deliberately hand-written rather than calling
// scheduler.Partition. See countGranted's comment on the ns parameter.
func countOccupying(t *testing.T, ns string) int {
	t.Helper()
	n := 0
	for _, e := range listEnvs(t, ns) {
		switch {
		case e.Status.Slot.Granted:
			n++
		case e.Status.Phase == sandboxv1alpha1.PhaseRestoring,
			e.Status.Phase == sandboxv1alpha1.PhaseRunning,
			e.Status.Phase == sandboxv1alpha1.PhaseFreezing:
			n++
		}
	}
	return n
}

// newSlotScheduler builds a SlotScheduler wired to the envtest suite's own
// k8s client for BOTH Client and Reader: envtest's direct client is already
// uncached (no informer in front of it), so it is a faithful test double
// for the production Reader (mgr.GetAPIReader()).
func newSlotScheduler(t *testing.T, capacity int, clk *fakeClock) *SlotScheduler {
	t.Helper()
	return &SlotScheduler{
		Client:   k8s,
		Reader:   k8s,
		Capacity: capacity,
		Interval: 50 * time.Millisecond,
		Clock:    clk.Now,
	}
}
