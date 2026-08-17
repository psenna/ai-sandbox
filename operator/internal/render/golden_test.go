package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"os"
	"path/filepath"
	"testing"

	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata/")

// assertGolden compares got against the golden file testdata/name, or
// rewrites it when -update is passed.
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: path is always filepath.Join("testdata", name) for a name literal passed by a test in this package
	if err != nil {
		t.Fatalf("reading golden file %s (run with -update to create it): %v", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("golden mismatch for %s (run with -update to refresh):\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// marshalForGolden marshals obj to YAML for a golden-file comparison.
func marshalForGolden(t *testing.T, obj any) []byte {
	t.Helper()
	b, err := yaml.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// redactSecretData replaces every value in a rendered Secret's data with a
// short, stable, non-reversible fingerprint before it is written to a golden
// file. Golden files under testdata/ are NOT covered by git-proxy's
// secret_scan ignore-paths (only *_test.go is), so a real-looking
// base64/token blob committed here could block a future push; this also
// means golden files stay byte-exact for keys/labels/ownerRefs/ordering
// regardless of what credential value a test happens to use. Actual secret
// VALUES are asserted directly in Go code (secret_test.go), never via golden
// comparison.
func redactSecretData(sec *acorev1.SecretApplyConfiguration) *acorev1.SecretApplyConfiguration {
	if sec == nil || sec.Data == nil {
		return sec
	}
	redacted := make(map[string][]byte, len(sec.Data))
	for k, v := range sec.Data {
		sum := sha256.Sum256(v)
		redacted[k] = []byte("sha256:" + hex.EncodeToString(sum[:])[:16])
	}
	out := *sec
	out.Data = redacted
	return &out
}
