package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

var unsafeNameChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeDiagName(name string) string {
	if name == "" {
		name = "unnamed"
	}
	return unsafeNameChars.ReplaceAllString(name, "_")
}

// DumpDiagnostics writes a best-effort debug bundle under
// Cfg.ArtifactDir/<sanitized name>/: operator pod logs (+previous),
// operator Deployment/Pods YAML, cluster-wide Nodes/SandboxClasses/
// NetworkPolicies/Events YAML, the given namespaces' SandboxEnvironments/
// Pods/Events/PVCs/ConfigMaps YAML, per-namespace pod logs (+previous),
// Secrets projected to NAME+KEY-NAMES ONLY (never decoded values -- see
// internal/controller/secretleak_test.go, the invariant this must not
// regress), the platform doubles' request logs, and an S3List dump of the
// snapshot bucket.
//
// namespaces empty means every non-"kube-*" namespace. Every step is
// best-effort: a failure writes an "<step>.error" file and moves on, it
// never panics or fails the calling spec.
func (h *Harness) DumpDiagnostics(ctx context.Context, name string, namespaces ...string) {
	dir := filepath.Join(h.Cfg.ArtifactDir, sanitizeDiagName(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return // nowhere to write to; give up quietly
	}

	writeYAML(dir, "operator-deployment.yaml", h.dumpDeployment(ctx, h.Cfg.OperatorNamespace, "ai-sandbox-operator-controller-manager"))
	h.dumpNamespacePods(ctx, dir, "operator", h.Cfg.OperatorNamespace, "control-plane=controller-manager")
	h.dumpOperatorMetrics(ctx, dir)

	writeYAML(dir, "nodes.yaml", h.dumpList(ctx, &corev1.NodeList{}))
	writeYAML(dir, "sandboxclasses.yaml", h.dumpList(ctx, &sandboxv1alpha1.SandboxClassList{}))
	writeYAML(dir, "networkpolicies.yaml", h.dumpList(ctx, &networkingv1.NetworkPolicyList{}))
	writeYAML(dir, "events-cluster.yaml", h.dumpList(ctx, &corev1.EventList{}))

	if len(namespaces) == 0 {
		namespaces = h.nonKubeNamespaces(ctx)
	}
	for _, ns := range namespaces {
		nsDir := filepath.Join(dir, "ns-"+ns)
		_ = os.MkdirAll(nsDir, 0o755)

		writeYAML(nsDir, "sandboxenvironments.yaml", h.dumpList(ctx, &sandboxv1alpha1.SandboxEnvironmentList{}, client.InNamespace(ns)))
		writeYAML(nsDir, "pods.yaml", h.dumpList(ctx, &corev1.PodList{}, client.InNamespace(ns)))
		writeYAML(nsDir, "events.yaml", h.dumpList(ctx, &corev1.EventList{}, client.InNamespace(ns)))
		writeYAML(nsDir, "pvcs.yaml", h.dumpList(ctx, &corev1.PersistentVolumeClaimList{}, client.InNamespace(ns)))
		writeYAML(nsDir, "configmaps.yaml", h.dumpList(ctx, &corev1.ConfigMapList{}, client.InNamespace(ns)))

		h.dumpSecretKeyNames(ctx, nsDir, ns)
		h.dumpNamespacePods(ctx, nsDir, "", ns, "")
	}

	h.dumpDoublesRequests(ctx, dir)
	h.dumpSnapshotBucket(ctx, dir)
}

func writeYAML(dir, filename string, v any) {
	if v == nil {
		return
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		_ = os.WriteFile(filepath.Join(dir, filename+".error"), []byte(err.Error()), 0o644)
		return
	}
	_ = os.WriteFile(filepath.Join(dir, filename), b, 0o644)
}

func (h *Harness) dumpList(ctx context.Context, list client.ObjectList, opts ...client.ListOption) any {
	if err := h.Client.List(ctx, list, opts...); err != nil {
		return map[string]string{"error": err.Error()}
	}
	return list
}

func (h *Harness) dumpDeployment(ctx context.Context, ns, name string) any {
	var d appsv1.Deployment
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &d); err != nil {
		return map[string]string{"error": err.Error()}
	}
	return &d
}

func (h *Harness) dumpNamespacePods(ctx context.Context, dir, subdir, ns, labelSelectorRaw string) {
	target := dir
	if subdir != "" {
		target = filepath.Join(dir, subdir)
		_ = os.MkdirAll(target, 0o755)
	}
	var pods corev1.PodList
	opts := []client.ListOption{client.InNamespace(ns)}
	if labelSelectorRaw != "" {
		sel, err := labels.Parse(labelSelectorRaw)
		if err == nil {
			opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
		}
	}
	if err := h.Client.List(ctx, &pods, opts...); err != nil {
		_ = os.WriteFile(filepath.Join(target, "pods-list.error"), []byte(err.Error()), 0o644)
		return
	}
	for _, p := range pods.Items {
		for _, c := range p.Spec.Containers {
			logs := h.PodLogs(ctx, ns, p.Name, c.Name, false)
			_ = os.WriteFile(filepath.Join(target, fmt.Sprintf("%s.%s.log", p.Name, c.Name)), []byte(logs), 0o644)
			prev := h.PodLogs(ctx, ns, p.Name, c.Name, true)
			_ = os.WriteFile(filepath.Join(target, fmt.Sprintf("%s.%s.previous.log", p.Name, c.Name)), []byte(prev), 0o644)
		}
	}
}

func (h *Harness) nonKubeNamespaces(ctx context.Context) []string {
	var nsList corev1.NamespaceList
	if err := h.Client.List(ctx, &nsList); err != nil {
		return nil
	}
	var out []string
	for _, ns := range nsList.Items {
		if strings.HasPrefix(ns.Name, "kube-") {
			continue
		}
		out = append(out, ns.Name)
	}
	return out
}

// secretKeyNames is the NAME+KEY-NAMES-ONLY projection: never decoded
// values, matching internal/controller/secretleak_test.go's invariant.
type secretKeyNames struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Keys      []string `json:"keys"`
}

func (h *Harness) dumpSecretKeyNames(ctx context.Context, dir, ns string) {
	var secrets corev1.SecretList
	if err := h.Client.List(ctx, &secrets, client.InNamespace(ns)); err != nil {
		return
	}
	var out []secretKeyNames
	for _, s := range secrets.Items {
		keys := make([]string, 0, len(s.Data))
		for k := range s.Data {
			keys = append(keys, k)
		}
		out = append(out, secretKeyNames{Namespace: s.Namespace, Name: s.Name, Keys: keys})
	}
	writeYAML(dir, "secrets-key-names.yaml", out)
}

// dumpDoublesRequests fetches each double's request log with a plain,
// non-failing HTTP GET (deliberately NOT h.BrokerRequests/h.ModelRequests,
// which fail the current spec on error via Gomega -- diagnostics must never
// do that).
func (h *Harness) dumpDoublesRequests(ctx context.Context, dir string) {
	writeJSON(filepath.Join(dir, "broker-requests.json"), fetchJSONBestEffort(ctx, h.brokerBaseURL()+"/_control/requests"))
	writeJSON(filepath.Join(dir, "model-requests.json"), fetchJSONBestEffort(ctx, h.modelBaseURL()+"/_control/requests"))
}

func fetchJSONBestEffort(ctx context.Context, url string) []RecordedRequest {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var out []RecordedRequest
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func (h *Harness) dumpSnapshotBucket(ctx context.Context, dir string) {
	objs, err := h.S3List(ctx, h.Cfg.SnapshotBucket, "")
	if err != nil {
		_ = os.WriteFile(filepath.Join(dir, "snapshot-bucket-list.error"), []byte(err.Error()), 0o644)
		return
	}
	writeJSON(filepath.Join(dir, "snapshot-bucket-list.json"), objs)
}

// dumpOperatorMetrics writes a best-effort raw dump of the operator's own
// /metrics scrape (#33) -- a plain text file, not YAML/JSON, since that is
// the format Prometheus text exposition already is and a human/CI artifact
// viewer can read it as-is (or feed it straight to ParsePromText for a
// post-mortem). Best-effort like every other step here: a scrape failure
// (operator pod not Running, proxy error) writes an .error file, never
// panics or fails the calling spec.
func (h *Harness) dumpOperatorMetrics(ctx context.Context, dir string) {
	text, err := ScrapeOperatorMetrics(ctx)
	if err != nil {
		_ = os.WriteFile(filepath.Join(dir, "operator-metrics.txt.error"), []byte(err.Error()), 0o644)
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "operator-metrics.txt"), []byte(text), 0o644)
}

func writeJSON(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o644)
}
