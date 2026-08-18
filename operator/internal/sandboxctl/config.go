package sandboxctl

import (
	"flag"
	"fmt"
	"net"
	"strings"
	"time"
)

const envPrefix = "SANDBOX_SIDECAR_"

// Config holds sandboxctl's runtime configuration, sourced from CLI flags
// (the `serve` subcommand's args) with environment-variable fallbacks.
type Config struct {
	Environment string
	Namespace   string
	Listen      string

	PollInterval    time.Duration
	ShutdownTimeout time.Duration
}

// Load parses args into a Config, falling back to the environment (via
// getenv) and then to built-in defaults for any flag not explicitly set.
func Load(args []string, getenv func(string) string) (Config, error) {
	var c Config

	fs := flag.NewFlagSet("sandboxctl serve", flag.ContinueOnError)
	fs.StringVar(&c.Environment, "environment", envOr(getenv, "ENVIRONMENT", ""),
		"name of the SandboxEnvironment this sidecar belongs to")
	fs.StringVar(&c.Namespace, "namespace", envOr(getenv, "NAMESPACE", ""),
		"namespace of the SandboxEnvironment this sidecar belongs to")
	fs.StringVar(&c.Listen, "listen", envOr(getenv, "LISTEN", "127.0.0.1:9099"),
		"loopback-only address the control API binds; a non-loopback host is rejected by Validate")
	fs.DurationVar(&c.PollInterval, "poll-interval", envOrDuration(getenv, "POLL_INTERVAL", 5*time.Second),
		"how often the sidecar polls its own SandboxEnvironment (the Role grants no watch)")
	fs.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", envOrDuration(getenv, "SHUTDOWN_TIMEOUT", 100*time.Second),
		"bounded deadline for graceful HTTP shutdown after SIGTERM/SIGINT; must sit inside the pod's terminationGracePeriodSeconds with headroom")

	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parsing flags: %w", err)
	}
	return c, nil
}

// Validate returns an error naming the first invalid field, or nil if c is
// well-formed. The loopback-only check on Listen is the structural
// enforcement of "unreachable from outside the pod": it makes it impossible
// for a future edit to accidentally widen the bind without Validate itself
// changing.
func (c Config) Validate() error {
	if c.Environment == "" {
		return fmt.Errorf("environment: must not be empty")
	}
	if c.Namespace == "" {
		return fmt.Errorf("namespace: must not be empty")
	}
	if err := validateLoopbackListen(c.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if c.PollInterval < time.Second || c.PollInterval > time.Minute {
		return fmt.Errorf("poll-interval: must be between 1s and 60s, got %s", c.PollInterval)
	}
	if c.ShutdownTimeout <= 0 || c.ShutdownTimeout > 110*time.Second {
		return fmt.Errorf("shutdown-timeout: must be between 0 and 110s (leave headroom inside the pod's terminationGracePeriodSeconds), got %s", c.ShutdownTimeout)
	}
	return nil
}

// validateLoopbackListen parses addr as a host:port pair and rejects any
// host that does not resolve to a loopback address. An empty host (e.g.
// ":9099" or "0.0.0.0:9099") is rejected explicitly and actionably, rather
// than silently defaulting to "all interfaces" the way net.Listen would.
func validateLoopbackListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not a valid host:port: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("%q has no port", addr)
	}
	if host == "" {
		return fmt.Errorf("%q has no host; must bind an explicit loopback address (e.g. 127.0.0.1:9099), not all interfaces", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("host %q is not an IP literal; must be an explicit loopback address (e.g. 127.0.0.1 or ::1)", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("host %q is not a loopback address; the control API must be reachable only from inside this pod", host)
	}
	return nil
}

func envOr(getenv func(string) string, name, def string) string {
	if v := getenv(envPrefix + name); v != "" {
		return v
	}
	return def
}

func envOrDuration(getenv func(string) string, name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(getenv(envPrefix + name))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
