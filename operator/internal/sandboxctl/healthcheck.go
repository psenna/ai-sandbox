package sandboxctl

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"time"
)

// Healthcheck implements the `sandboxctl healthcheck` subcommand: dial
// http://<listen>/healthz and return nil on a 200, an error otherwise (main
// maps that to exit code 0/1). Used as the pod's startupProbe
// (`exec: ["/sandboxctl", "healthcheck"]`) because the sidecar binds
// loopback-only -- a kubelet httpGet probe dials the pod IP, not loopback,
// so it can never reach this listener -- and the distroless image has no
// shell, so the probe must be this binary, not curl/wget.
func Healthcheck(args []string, getenv func(string) string) error {
	fs := flag.NewFlagSet("sandboxctl healthcheck", flag.ContinueOnError)
	listen := fs.String("listen", envOr(getenv, "LISTEN", "127.0.0.1:9099"), "address to probe /healthz on")
	timeout := fs.Duration("timeout", 2*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// G107: url is built from a fixed loopback address only -- the
	// operator's own --listen flag/env default, matching what `serve` was
	// started with, never agent-controlled input.
	url := fmt.Sprintf("http://%s/healthz", *listen) //nolint:gosec
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building healthcheck request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned status %d, want %d", resp.StatusCode, http.StatusOK)
	}
	return nil
}
