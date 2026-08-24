package sandboxctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	utilexec "k8s.io/client-go/util/exec"
)

// fakeExecer is a minimal Execer double for exercising handleExec without a
// real Kubernetes client or SPDY.
type fakeExecer struct {
	stdout, stderr []byte
	err            error
	gotPod         string
	gotCmd         []string
	gotStdin       []byte
}

func (f *fakeExecer) Exec(_ context.Context, pod string, cmd []string, stdin []byte) ([]byte, []byte, error) {
	f.gotPod, f.gotCmd, f.gotStdin = pod, cmd, stdin
	return f.stdout, f.stderr, f.err
}

// newExecTestServer builds a control API server wired with the given Execer
// (nil makes /v1/exec 404, mirroring the services helper).
func newExecTestServer(t *testing.T, execer Execer) *httptest.Server {
	t.Helper()
	return newExecTestServerWithEnv(t, execer, EnvironmentRef{Name: "env-1", Namespace: "ns-1"})
}

func newExecTestServerWithEnv(t *testing.T, execer Execer, env EnvironmentRef) *httptest.Server {
	t.Helper()
	store := &fakeStore{}
	poll := NewPoller(store, time.Second, nil, nil)
	srv := NewServer(Config{Listen: "127.0.0.1:0"}, store, poll, env, nil, execer, time.Now, nil)
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestHandleExec_HappyPath(t *testing.T) {
	execer := &fakeExecer{stdout: []byte("hello\n")}
	ts := newExecTestServer(t, execer)

	body := `{"runtime":"python","command":["echo","hi"],"stdin":"pipe"}`
	resp, err := http.Post(ts.URL+"/v1/exec", "application/json", strings.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var er ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.Stdout != "hello\n" {
		t.Errorf("Stdout = %q, want %q", er.Stdout, "hello\n")
	}
	if er.Runtime != "python" {
		t.Errorf("Runtime = %q, want python", er.Runtime)
	}
	if er.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", er.ExitCode)
	}
	if er.Error != "" {
		t.Errorf("Error = %q, want empty on success", er.Error)
	}
	if execer.gotPod != "python" {
		t.Errorf("execer got pod = %q, want python", execer.gotPod)
	}
	if len(execer.gotCmd) != 2 || execer.gotCmd[0] != "echo" {
		t.Errorf("execer got cmd = %v, want [echo hi]", execer.gotCmd)
	}
	if string(execer.gotStdin) != "pipe" {
		t.Errorf("execer got stdin = %q, want pipe", string(execer.gotStdin))
	}
}

func TestHandleExec_EmptyRuntimeReturns400(t *testing.T) {
	execer := &fakeExecer{stdout: []byte("ok")}
	ts := newExecTestServer(t, execer)

	body := `{"runtime":"","command":["echo","hi"]}`
	resp, err := http.Post(ts.URL+"/v1/exec", "application/json", strings.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error.Code != CodeMissingParam {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeMissingParam)
	}
}

func TestHandleExec_EmptyCommandReturns400(t *testing.T) {
	execer := &fakeExecer{stdout: []byte("ok")}
	ts := newExecTestServer(t, execer)

	body := `{"runtime":"python","command":[]}`
	resp, err := http.Post(ts.URL+"/v1/exec", "application/json", strings.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error.Code != CodeMissingParam {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeMissingParam)
	}
}

func TestHandleExec_TransportErrorReturns200WithError(t *testing.T) {
	execer := &fakeExecer{err: errors.New("pod unreachable")}
	ts := newExecTestServer(t, execer)

	body := `{"runtime":"python","command":["ls"]}`
	resp, err := http.Post(ts.URL+"/v1/exec", "application/json", strings.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// A transport failure is still a 200: the response carries the error
	// message so the agent sees it alongside any partial output.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var er ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for a transport failure", er.ExitCode)
	}
	if er.Error == "" {
		t.Error("Error = empty, want the transport error message")
	}
	if !strings.Contains(er.Error, "pod unreachable") {
		t.Errorf("Error = %q, want it to contain the cause", er.Error)
	}
}

func TestHandleExec_NotEnabledReturns404(t *testing.T) {
	// nil execer => exec not enabled (non-k8s-native env). The handler
	// returns a clean 404, mirroring handleServicesApply's nil-sets path.
	ts := newExecTestServer(t, nil)

	body := `{"runtime":"python","command":["ls"]}`
	resp, err := http.Post(ts.URL+"/v1/exec", "application/json", strings.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error.Code != CodeNotFound {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeNotFound)
	}
}

func TestExtractExitCode(t *testing.T) {
	if code := extractExitCode(nil); code != 0 {
		t.Errorf("nil -> %d, want 0", code)
	}
	// A real non-zero exit surfaces as a utilexec.CodeExitError value
	// (remotecommand returns the value, not a pointer -- see v4.go).
	if code := extractExitCode(utilexec.CodeExitError{Err: errors.New("exit 3"), Code: 3}); code != 3 {
		t.Errorf("CodeExitError{Code:3} -> %d, want 3", code)
	}
	// A wrapped CodeExitError must still be matched via errors.As.
	wrapped := fmt.Errorf("wrapped: %w", utilexec.CodeExitError{Err: errors.New("exit 7"), Code: 7})
	if code := extractExitCode(wrapped); code != 7 {
		t.Errorf("wrapped CodeExitError{Code:7} -> %d, want 7", code)
	}
	// A plain transport error (no exit code) -> -1.
	if code := extractExitCode(errors.New("connection refused")); code != -1 {
		t.Errorf("transport error -> %d, want -1", code)
	}
}
