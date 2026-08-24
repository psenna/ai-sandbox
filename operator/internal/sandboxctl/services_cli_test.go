package sandboxctl

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validServicesYAML = `
services:
  - name: postgres
    image: postgres:18-alpine
runtimes:
  - name: python
    image: python:3.13-slim
    command: ["sleep", "infinity"]
`

// writeTempYAML writes content to a temp file and returns its path.
func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "services.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return path
}

// emptyGetenv returns "" for every env var so the --listen default does not
// pick up a real SANDBOX_SIDECAR_LISTEN from the environment.
func emptyGetenv(string) string { return "" }

// captureStderr swaps os.Stderr for a pipe, runs fn, and returns fn's exit
// code together with whatever was written to stderr. RunServicesApply writes
// its error/diagnostic lines to os.Stderr directly (matching main.go's
// convention), so this is the only way to assert on them. These tests do not
// run in parallel, so the global swap is safe.
func captureStderr(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	code := fn()
	_ = w.Close()
	os.Stderr = origStderr
	buf, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr pipe: %v", err)
	}
	return code, string(buf)
}

func TestRunServicesApply_HappyPath(t *testing.T) {
	var gotReq ServicesApplyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/services" || r.Method != http.MethodPost {
			t.Errorf("request %s %s, want POST /v1/services", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writeJSON(w, http.StatusOK, ServicesApplyResponse{
			Environment: "env-1", Services: 1, Runtimes: 1, Applied: true,
		})
	}))
	defer srv.Close()

	path := writeTempYAML(t, validServicesYAML)
	var out bytes.Buffer
	code := RunServicesApply([]string{"--listen", srv.Listener.Addr().String(), path}, emptyGetenv, &out)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "applied") {
		t.Errorf("stdout = %q, want it to contain 'applied'", out.String())
	}
	if !strings.Contains(out.String(), "env-1") {
		t.Errorf("stdout = %q, want it to contain the environment name", out.String())
	}
	if len(gotReq.Services) != 1 || gotReq.Services[0].Name != "postgres" {
		t.Errorf("posted services = %+v, want one postgres", gotReq.Services)
	}
	if len(gotReq.Runtimes) != 1 || gotReq.Runtimes[0].Name != "python" {
		t.Errorf("posted runtimes = %+v, want one python", gotReq.Runtimes)
	}
}

func TestRunServicesApply_ValidationErrorExits2(t *testing.T) {
	// A cross-list collision is caught client-side before any HTTP call, so
	// the server is never hit.
	path := writeTempYAML(t, `
services:
  - name: shared
    image: a
runtimes:
  - name: shared
    image: b
`)
	var out bytes.Buffer
	code, stderr := captureStderr(t, func() int {
		return RunServicesApply([]string{"--listen", "127.0.0.1:9099", path}, emptyGetenv, &out)
	})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for a client-side validation error", code)
	}
	if !strings.Contains(stderr, "invalid declaration") {
		t.Errorf("stderr = %q, want it to contain 'invalid declaration'", stderr)
	}
}

func TestRunServicesApply_Server422Exits1(t *testing.T) {
	// The server rejects with a 422 duplicate_entry_name envelope; the CLI
	// surfaces the control-API error and exits 1 (a runtime failure, not a
	// usage error).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusUnprocessableEntity, CodeDuplicateEntryName, "name \"shared\" appears in both service and runtime", "name", nil)
	}))
	defer srv.Close()

	// Use a valid declaration so the client-side pre-check passes and the
	// request reaches the server, which then rejects with 422.
	path := writeTempYAML(t, validServicesYAML)
	var out bytes.Buffer
	code, stderr := captureStderr(t, func() int {
		return RunServicesApply([]string{"--listen", srv.Listener.Addr().String(), path}, emptyGetenv, &out)
	})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for a control-API error", code)
	}
	if !strings.Contains(stderr, "services apply failed") {
		t.Errorf("stderr = %q, want it to contain 'services apply failed'", stderr)
	}
	if !strings.Contains(stderr, "shared") {
		t.Errorf("stderr = %q, want it to surface the server's message naming 'shared'", stderr)
	}
}

func TestRunServicesApply_NonLoopbackListenRejected(t *testing.T) {
	path := writeTempYAML(t, validServicesYAML)
	var out bytes.Buffer
	code, stderr := captureStderr(t, func() int {
		return RunServicesApply([]string{"--listen", "0.0.0.0:9099", path}, emptyGetenv, &out)
	})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for a non-loopback --listen", code)
	}
	if !strings.Contains(stderr, "invalid --listen") {
		t.Errorf("stderr = %q, want it to contain 'invalid --listen'", stderr)
	}
}

func TestRunServicesCompose_RendersToStdout(t *testing.T) {
	path := writeTempYAML(t, validServicesYAML)
	var out bytes.Buffer
	code := RunServicesCompose([]string{path}, emptyGetenv, &out)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "postgres:18-alpine") {
		t.Errorf("stdout = %q, want it to contain the image", out.String())
	}
	if !strings.Contains(out.String(), "python:3.13-slim") {
		t.Errorf("stdout = %q, want it to contain the runtime image", out.String())
	}
}

func TestRunServicesCompose_WritesToFile(t *testing.T) {
	path := writeTempYAML(t, validServicesYAML)
	dir := t.TempDir()
	out := filepath.Join(dir, "compose.yml")
	var stdout bytes.Buffer
	// Flags must precede the positional file argument: Go's flag package
	// stops parsing flags at the first non-flag argument.
	code := RunServicesCompose([]string{"-o", out, path}, emptyGetenv, &stdout)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty when -o is set", stdout.String())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	if !strings.Contains(string(b), "postgres:18-alpine") {
		t.Errorf("output file = %q, want it to contain the image", string(b))
	}
}
