// Package apitest holds envtest-backed validation and round-trip tests for
// the sandbox.psenna.dev/v1alpha1 API types. It is a separate package from
// api/v1alpha1 so that package carries no test-only imports (in particular,
// no envtest/client-go dependency edges beyond what the types themselves
// need).
//
// The suite is skipped unless KUBEBUILDER_ASSETS is set (see
// operator/hack/fetch-envtest.sh and `make envtest-assets`), since it needs
// real kube-apiserver/etcd binaries to install the CRDs against and
// round-trip objects through.
package apitest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

var (
	testEnv *envtest.Environment
	k8s     client.Client
	k8sCfg  *rest.Config
	ctx     = context.Background()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "skipping API envtest suite: KUBEBUILDER_ASSETS not set")
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
	_ = sandboxv1alpha1.AddToScheme(scheme)

	k8s, err = client.New(k8sCfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	// envtest runs kube-apiserver without kube-controller-manager, so the
	// default/kube-system/etc. namespaces it normally bootstraps don't
	// exist. Namespaced fixtures in this suite use "default"; create it
	// explicitly.
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
