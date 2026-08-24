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
