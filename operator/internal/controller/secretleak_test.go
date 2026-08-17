package controller

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/psenna/ai-sandbox/operator/internal/render"
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
