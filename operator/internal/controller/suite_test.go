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
	corev1 "k8s.io/api/core/v1"
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
	_ = authorizationv1.AddToScheme(scheme)
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

// mustCreateClass creates a minimal valid SandboxClass named "default" with
// default timeouts. SandboxClass is cluster-scoped, so name must be unique
// per test; callers that need more than one class should build their own.
func mustCreateClass(t *testing.T) *sandboxv1alpha1.SandboxClass {
	t.Helper()
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

// mustCreateClassWithGitProxy creates a minimal valid SandboxClass named
// "default" whose services.gitProxy.tokenSecretRef points at secretName/key
// in secretNamespace -- the class-referenced Secret D3 describes.
func mustCreateClassWithGitProxy(t *testing.T, secretNamespace, secretName, key string) *sandboxv1alpha1.SandboxClass {
	t.Helper()
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
