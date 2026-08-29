package sandboxctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// Execer runs a one-shot command inside a runtime pod. The container is
// implicit: the ServiceSetReconciler names a runtime pod's single container
// after the entry name (== podName), so podName doubles as the container name.
// Interactive/streaming/TTY exec is deferred; this is run-command-get-output.
type Execer interface {
	Exec(ctx context.Context, podName string, cmd []string, stdin []byte) (stdout, stderr []byte, err error)
}

// podExecer is the real Execer: a client-go SPDY exec, mirroring
// test/e2e/harness.go's Exec exactly, with stdin piped from a bytes.Reader.
type podExecer struct {
	restCfg   *rest.Config
	namespace string
}

func newPodExecer(restCfg *rest.Config, namespace string) *podExecer {
	return &podExecer{restCfg: restCfg, namespace: namespace}
}

func (e *podExecer) Exec(ctx context.Context, podName string, cmd []string, stdin []byte) ([]byte, []byte, error) {
	cs, err := kubernetes.NewForConfig(e.restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("building clientset: %w", err)
	}
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(e.namespace).
		Name(podName).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: podName,
		Command:   cmd,
		Stdin:     len(stdin) > 0,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(e.restCfg, "POST", req.URL())
	if err != nil {
		return nil, nil, fmt.Errorf("building SPDY executor: %w", err)
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	opts := remotecommand.StreamOptions{Stdout: &stdoutBuf, Stderr: &stderrBuf}
	if len(stdin) > 0 {
		opts.Stdin = bytes.NewReader(stdin)
	}
	err = executor.StreamWithContext(ctx, opts)
	return stdoutBuf.Bytes(), stderrBuf.Bytes(), err
}

// extractExitCode best-effort-extracts a command exit code from a remotecommand
// error. remotecommand surfaces a non-zero exit as a utilexec.CodeExitError
// (a VALUE -- see tools/remotecommand/v4.go, which returns exec.CodeExitError{}
// directly, not a pointer), so errors.As must target the value type to match.
// ExitStatus() reads the Code field without coupling to it directly. Anything
// that is not a CodeExitError is a transport/protocol failure (returns -1).
func extractExitCode(err error) int {
	if err == nil {
		return 0
	}
	var cee utilexec.CodeExitError
	if errors.As(err, &cee) {
		return cee.ExitStatus()
	}
	return -1
}
