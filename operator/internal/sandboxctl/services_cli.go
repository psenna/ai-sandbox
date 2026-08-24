package sandboxctl

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
)

// RunServicesApply implements `sandboxctl services apply [flags] [file]`.
// Default file is ./services.yaml. It parses + validates client-side, POSTs to
// the loopback control API, and prints the result. A validation error is
// printed to stderr and exits 2; a control-API error exits 1.
func RunServicesApply(args []string, getenv func(string) string, out io.Writer) int {
	fs := newFlagSet("sandboxctl services apply")
	listen := fs.String("listen", envOr(getenv, "LISTEN", "127.0.0.1:9099"), "control API address")
	file := fs.String("file", "", "path to services.yaml (default ./services.yaml)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "invalid flags: "+err.Error())
		return 2
	}
	if err := validateLoopbackListen(*listen); err != nil {
		fmt.Fprintln(os.Stderr, "invalid --listen: "+err.Error())
		return 2
	}
	path := *file
	if path == "" {
		if fs.NArg() > 0 {
			path = fs.Arg(0)
		} else {
			path = "services.yaml"
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading "+path+": "+err.Error())
		return 2
	}
	spec, err := ParseServicesYAML(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parsing "+path+": "+err.Error())
		return 2
	}
	spec.EnvironmentName = "local" // client-side pre-check placeholder; server overrides
	if err := ValidateServiceSet(spec); err != nil {
		fmt.Fprintln(os.Stderr, "invalid declaration: "+err.Error())
		return 2
	}
	cli := NewControlClient(*listen)
	resp, err := cli.ApplyServices(context.Background(), ServicesApplyRequest{Services: spec.Services, Runtimes: spec.Runtimes})
	if err != nil {
		fmt.Fprintln(os.Stderr, "services apply failed: "+err.Error())
		return 1
	}
	fmt.Fprintf(out, "applied environment=%s services=%d runtimes=%d\n", resp.Environment, resp.Services, resp.Runtimes)
	return 0
}

// RunServicesCompose implements `sandboxctl services compose [flags] [file]`.
// Pure: renders docker-compose.yml to stdout (or -o).
func RunServicesCompose(args []string, getenv func(string) string, out io.Writer) int {
	fs := newFlagSet("sandboxctl services compose")
	file := fs.String("file", "", "path to services.yaml (default ./services.yaml)")
	output := fs.String("o", "", "write to file instead of stdout")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "invalid flags: "+err.Error())
		return 2
	}
	path := *file
	if path == "" {
		if fs.NArg() > 0 {
			path = fs.Arg(0)
		} else {
			path = "services.yaml"
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading "+path+": "+err.Error())
		return 2
	}
	spec, err := ParseServicesYAML(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parsing "+path+": "+err.Error())
		return 2
	}
	rendered, err := Compose(spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rendering compose: "+err.Error())
		return 1
	}
	if *output == "" {
		_, _ = out.Write(rendered)
		return 0
	}
	if err := os.WriteFile(*output, rendered, 0o644); err != nil { //nolint:gosec // G306: compose YAML, non-secret
		fmt.Fprintln(os.Stderr, "writing "+*output+": "+err.Error())
		return 1
	}
	return 0
}

// newFlagSet mirrors config.go's flag.NewFlagSet usage with a consistent name
// and ContinueOnError.
func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

// RunExec implements `sandboxctl exec [flags] <runtime> -- <cmd...>`.
// Stdin is read from the passed stdin reader and sent as text. The command's
// stdout/stderr are written to the respective writers; the process exit code is
// mapped from the response's best-effort ExitCode (-1 -> 1, Error set -> 1).
// All CLI diagnostics (usage, flag errors, transport failures) are written to
// the passed stderr writer so tests can capture them without a global os.Stderr
// swap (unlike RunServicesApply, which predates the stderr parameter).
func RunExec(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("sandboxctl exec")
	listen := fs.String("listen", envOr(getenv, "LISTEN", "127.0.0.1:9099"), "control API address")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, "invalid flags: "+err.Error())
		return 2
	}
	if err := validateLoopbackListen(*listen); err != nil {
		fmt.Fprintln(stderr, "invalid --listen: "+err.Error())
		return 2
	}
	rest := fs.Args()
	// Expect: <runtime> -- <cmd...>
	if len(rest) < 1 {
		fmt.Fprintln(stderr, "usage: sandboxctl exec <runtime> -- <cmd...>")
		return 2
	}
	runtime := rest[0]
	cmd := rest[1:]
	// Drop a leading "--" separator if present.
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 {
		fmt.Fprintln(stderr, "usage: sandboxctl exec <runtime> -- <cmd...>")
		return 2
	}
	// Read stdin fully when the caller provides a non-empty reader (the test
	// passes a strings.NewReader; in real use os.Stdin is a pipe/file). A nil
	// or empty stdin sends no stdin to the pod.
	var stdinBytes []byte
	if stdin != nil {
		if b, err := io.ReadAll(stdin); err == nil {
			stdinBytes = b
		}
	}
	cli := NewControlClient(*listen)
	resp, err := cli.Exec(context.Background(), ExecRequest{Runtime: runtime, Command: cmd, Stdin: string(stdinBytes)})
	if err != nil {
		fmt.Fprintln(stderr, "exec failed: "+err.Error())
		return 1
	}
	_, _ = stdout.Write([]byte(resp.Stdout))
	_, _ = stderr.Write([]byte(resp.Stderr))
	if resp.Error != "" {
		fmt.Fprintln(stderr, "exec error: "+resp.Error)
		return 1
	}
	if resp.ExitCode < 0 {
		return 1
	}
	return resp.ExitCode
}
