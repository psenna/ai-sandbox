package controller

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// sentinelToken is a deliberately low-entropy, obviously-fake bearer value.
// If this string ever shows up somewhere it shouldn't (a log line, an
// Event, a status field, a ConfigMap), that is unambiguously a leak, not a
// coincidence.
const sentinelToken = "leak-canary-git-proxy-token-19" //nolint:gosec // G101: deliberately fake, low-entropy sentinel value used to detect secret leaks, not a real credential

// TestNoSecretLeak_LogsEventsStatusConfigMap implements D4: a full
// reconcile with a real class-referenced Secret containing sentinelToken,
// and assertions that the sentinel appears in NONE of: captured logs, every
// Event in both the environment namespace and the class-secret namespace,
// the environment's serialized status, or the ConfigMap's data. A positive
// control asserts the sentinel DOES appear in Secret.Data["GIT_PROXY_TOKEN"],
// proving the test isn't vacuous.
func TestNoSecretLeak_LogsEventsStatusConfigMap(t *testing.T) {
	const classSecretNS = "default"
	mustCreateSourceSecret(t, classSecretNS, "git-proxy-token-leak", "token", sentinelToken)
	mustCreateClassWithGitProxy(t, classSecretNS, "git-proxy-token-leak", "token")
	env := mustCreateEnv(t, "secret-leak-check")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	capture, logger := newLogCapture()
	capturedCtx := logf.IntoContext(ctx, logger)

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	if _, err := r.Reconcile(capturedCtx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// A second reconcile so the resources exist and observeCluster's
	// resolveCredentials path (which also touches the source Secret) runs
	// too.
	if _, err := r.Reconcile(capturedCtx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile (2nd): %v", err)
	}

	if capture.Contains(sentinelToken) {
		t.Errorf("sentinel token leaked into captured logs:\n%s", capture.String())
	}

	for _, ns := range []string{env.Namespace, classSecretNS} {
		var events corev1.EventList
		if err := k8s.List(ctx, &events, client.InNamespace(ns)); err != nil {
			t.Fatalf("listing events in %s: %v", ns, err)
		}
		for _, e := range events.Items {
			if strings.Contains(e.Message, sentinelToken) || strings.Contains(e.Reason, sentinelToken) {
				t.Errorf("sentinel token leaked into Event %s/%s: %+v", e.Namespace, e.Name, e)
			}
		}
	}

	got := getEnv(t, key)
	statusJSON, err := json.Marshal(got.Status)
	if err != nil {
		t.Fatalf("marshaling status: %v", err)
	}
	if strings.Contains(string(statusJSON), sentinelToken) {
		t.Errorf("sentinel token leaked into environment status:\n%s", statusJSON)
	}

	names := render.ChildNames(env.Name)
	var cm corev1.ConfigMap
	mustGetObj(t, env.Namespace, names.ConfigMap, &cm)
	for k, v := range cm.Data {
		if strings.Contains(v, sentinelToken) {
			t.Errorf("sentinel token leaked into ConfigMap data key %q: %q", k, v)
		}
	}

	// Positive control: the sentinel MUST appear in the rendered Secret --
	// otherwise this whole test would be vacuously true.
	var secret corev1.Secret
	mustGetObj(t, env.Namespace, names.Secret, &secret)
	if string(secret.Data["GIT_PROXY_TOKEN"]) != sentinelToken {
		t.Fatalf("positive control failed: Secret.Data[GIT_PROXY_TOKEN] = %q, want sentinel %q (test would be vacuous otherwise)", secret.Data["GIT_PROXY_TOKEN"], sentinelToken)
	}
}

// s3SentinelSecretAccessKey is a SECOND, distinct sentinel value (#28): the
// S3 snapshot secretAccessKey resolved by resolveCredentials. It must
// appear ONLY in the rendered <env>-snapshot Secret's secretAccessKey data
// key, and nowhere else (no log, Event, status, ConfigMap, or the existing
// <env>-env Secret).
const s3SentinelSecretAccessKey = "leak-canary-s3-secret-access-key-77" //nolint:gosec // G101: deliberately fake, low-entropy sentinel value, not a real credential

func TestNoSecretLeak_S3SnapshotCredentialsOnlyInSnapshotSecret(t *testing.T) {
	const classSecretNS = "default"
	const credsSecretName = "s3-creds-leak-check" //nolint:gosec // G101: a Secret NAME, not a credential value

	sourceSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credsSecretName, Namespace: classSecretNS},
		Data: map[string][]byte{
			storage.SecretKeyAccessKeyID:     []byte("fake-access-key-id-leak-check"),
			storage.SecretKeySecretAccessKey: []byte(s3SentinelSecretAccessKey),
		},
	}
	if err := k8s.Create(ctx, sourceSecret); err != nil {
		t.Fatalf("creating source s3 creds secret: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, sourceSecret) })

	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Storage: sandboxv1alpha1.StorageSpec{
				Backend: sandboxv1alpha1.BackendSpec{
					Type: sandboxv1alpha1.StorageBackendTypeS3,
					S3: &sandboxv1alpha1.S3Backend{
						Endpoint:             "https://s3.example.com",
						Bucket:               "sandbox-snapshots",
						CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{Name: credsSecretName},
					},
				},
			},
		},
	}
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, class) })

	env := mustCreateEnv(t, "s3-secret-leak-check")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	capture, logger := newLogCapture()
	capturedCtx := logf.IntoContext(ctx, logger)

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	if _, err := r.Reconcile(capturedCtx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := r.Reconcile(capturedCtx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile (2nd): %v", err)
	}

	if capture.Contains(s3SentinelSecretAccessKey) {
		t.Errorf("s3 sentinel leaked into captured logs:\n%s", capture.String())
	}

	for _, ns := range []string{env.Namespace, classSecretNS} {
		var events corev1.EventList
		if err := k8s.List(ctx, &events, client.InNamespace(ns)); err != nil {
			t.Fatalf("listing events in %s: %v", ns, err)
		}
		for _, e := range events.Items {
			if strings.Contains(e.Message, s3SentinelSecretAccessKey) || strings.Contains(e.Reason, s3SentinelSecretAccessKey) {
				t.Errorf("s3 sentinel leaked into Event %s/%s: %+v", e.Namespace, e.Name, e)
			}
		}
	}

	got := getEnv(t, key)
	statusJSON, err := json.Marshal(got.Status)
	if err != nil {
		t.Fatalf("marshaling status: %v", err)
	}
	if strings.Contains(string(statusJSON), s3SentinelSecretAccessKey) {
		t.Errorf("s3 sentinel leaked into environment status:\n%s", statusJSON)
	}

	names := render.ChildNames(env.Name)
	var cm corev1.ConfigMap
	mustGetObj(t, env.Namespace, names.ConfigMap, &cm)
	for k, v := range cm.Data {
		if strings.Contains(v, s3SentinelSecretAccessKey) {
			t.Errorf("s3 sentinel leaked into ConfigMap data key %q: %q", k, v)
		}
	}

	var envSecret corev1.Secret
	mustGetObj(t, env.Namespace, names.Secret, &envSecret)
	for k, v := range envSecret.Data {
		if strings.Contains(string(v), s3SentinelSecretAccessKey) {
			t.Errorf("s3 sentinel leaked into the <env>-env Secret's key %q", k)
		}
	}

	var snapSecret corev1.Secret
	mustGetObj(t, env.Namespace, names.SnapshotSecret, &snapSecret)
	for k, v := range snapSecret.Data {
		if k == "secretAccessKey" {
			continue
		}
		if strings.Contains(string(v), s3SentinelSecretAccessKey) {
			t.Errorf("s3 sentinel appeared in unexpected snapshot secret key %q", k)
		}
	}
	// Positive control: the sentinel MUST appear in exactly the
	// <env>-snapshot Secret's secretAccessKey key -- otherwise this whole
	// test would be vacuously true.
	if string(snapSecret.Data["secretAccessKey"]) != s3SentinelSecretAccessKey {
		t.Fatalf("positive control failed: SnapshotSecret.Data[secretAccessKey] = %q, want sentinel %q (test would be vacuous otherwise)",
			snapSecret.Data["secretAccessKey"], s3SentinelSecretAccessKey)
	}
}
