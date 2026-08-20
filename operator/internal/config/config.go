// Package config defines the operator's configuration surface: flags with
// environment-variable fallbacks, and validation of the resulting values.
package config

import (
	"flag"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const envPrefix = "SANDBOX_OPERATOR_"

// Config holds the operator's runtime configuration, sourced from CLI flags
// with environment-variable fallbacks (flag > env > built-in default).
type Config struct {
	SlotCapacity        int
	ClusterID           string
	WatchNamespace      string // "" = all namespaces
	DefaultSandboxClass string

	// ClassSecretNamespace is the namespace holding the Secrets referenced by
	// a SandboxClass (e.g. services.gitProxy.tokenSecretRef). The operator
	// reads them and projects only the values an environment needs into a
	// new Secret it owns in the environment's own namespace.
	ClassSecretNamespace string

	MetricsAddr     string // "0" disables the metrics listener
	HealthProbeAddr string // "0" disables the health/readiness listener

	EnableLeaderElection    bool
	LeaderElectionID        string
	LeaderElectionNamespace string

	// SchedulerInterval is how often the slot scheduler runs an admission
	// pass. Each pass performs one live LIST of SandboxEnvironments across
	// the watch scope, so this trades admission latency against API-server
	// load.
	SchedulerInterval time.Duration

	// WarmCacheGCInterval is how often the warm-cache GC runs a reclamation
	// pass (#29). Each pass performs one live LIST of SandboxEnvironments
	// across the watch scope plus a Get per distinct class, so this trades
	// warm-cache reclamation latency against API-server load.
	WarmCacheGCInterval time.Duration

	// SidecarImage is the container image for the always-present
	// sandboxctl control-channel sidecar (#27). It is the operator's OWN
	// image (the sandboxctl binary ships alongside the manager binary --
	// see operator/Dockerfile), never taken from a SandboxClass. Must be
	// kept in sync with the deployed operator image tag -- see
	// operator/README.md.
	SidecarImage string

	// OperatorIngressLabel is the single "key=value" label selector
	// identifying the operator's own pods, allowed to reach sandbox pods
	// under Restricted isolation (#31). Defaults to
	// "control-plane=controller-manager" (the label the operator's own
	// Deployment carries -- see config/manager/manager.yaml).
	OperatorIngressLabel string

	// CNIProbeInterval is how often the CNI enforcement probe runs a pass
	// (#31). Each pass creates two short-lived probe pods in the operator's
	// own namespace, so this trades enforcement-detection latency against
	// API-server load.
	CNIProbeInterval time.Duration
}

// dns1123LabelRE matches a valid DNS-1123 label: lowercase alphanumeric
// characters or '-', starting and ending with an alphanumeric character.
var dns1123LabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Load parses args (typically os.Args[1:]) into a Config, falling back to
// the environment (via getenv) and then to built-in defaults for any flag
// not explicitly set on the command line.
func Load(args []string, getenv func(string) string) (Config, error) {
	var c Config

	fs := flag.NewFlagSet("operator", flag.ContinueOnError)

	fs.IntVar(&c.SlotCapacity, "slot-capacity",
		envOrInt(getenv, "SLOT_CAPACITY", 4), "maximum number of sandbox slots this node/cluster may run concurrently")
	fs.StringVar(&c.ClusterID, "cluster-id",
		envOr(getenv, "CLUSTER_ID", "default"), "identifier for this cluster, used as a storage path segment")
	fs.StringVar(&c.WatchNamespace, "watch-namespace",
		envOr(getenv, "WATCH_NAMESPACE", ""), "namespace to restrict watches to; empty watches all namespaces")
	fs.StringVar(&c.DefaultSandboxClass, "default-sandbox-class",
		envOr(getenv, "DEFAULT_SANDBOX_CLASS", "default"), "name of the SandboxClass used when none is specified")
	fs.StringVar(&c.MetricsAddr, "metrics-bind-address",
		envOr(getenv, "METRICS_BIND_ADDRESS", ":8080"), "address the metrics endpoint binds to; \"0\" disables it")
	fs.StringVar(&c.HealthProbeAddr, "health-probe-bind-address",
		envOr(getenv, "HEALTH_PROBE_BIND_ADDRESS", ":8081"), "address the health/readiness probe endpoint binds to; \"0\" disables it")
	fs.BoolVar(&c.EnableLeaderElection, "leader-elect",
		envOrBool(getenv, "LEADER_ELECT", true), "enable leader election for controller manager")
	fs.StringVar(&c.LeaderElectionID, "leader-election-id",
		envOr(getenv, "LEADER_ELECTION_ID", "sandbox-operator.sandbox.psenna.dev"), "name of the resource used for leader election")
	fs.StringVar(&c.LeaderElectionNamespace, "leader-election-namespace",
		envOr(getenv, "LEADER_ELECTION_NAMESPACE", ""), "namespace used for leader election; empty uses the in-cluster namespace")
	fs.StringVar(&c.ClassSecretNamespace, "class-secret-namespace",
		classSecretNamespaceDefault(getenv),
		"namespace holding the Secrets referenced by SandboxClass (e.g. services.gitProxy.tokenSecretRef); the operator reads them and projects only the values an environment needs")
	fs.DurationVar(&c.SchedulerInterval, "scheduler-interval",
		envOrDuration(getenv, "SCHEDULER_INTERVAL", 5*time.Second),
		"how often the slot scheduler runs an admission pass")
	fs.DurationVar(&c.WarmCacheGCInterval, "warm-cache-gc-interval",
		envOrDuration(getenv, "WARM_CACHE_GC_INTERVAL", time.Minute),
		"how often the warm-cache GC runs a reclamation pass")
	fs.StringVar(&c.SidecarImage, "sidecar-image",
		envOr(getenv, "SIDECAR_IMAGE", "ghcr.io/psenna/ai-sandbox-operator:dev"),
		"container image for the always-present sandboxctl control-channel sidecar; must match the deployed operator image tag")
	fs.StringVar(&c.OperatorIngressLabel, "operator-ingress-label",
		envOr(getenv, "OPERATOR_INGRESS_LABEL", "control-plane=controller-manager"),
		"single key=value label selector identifying the operator's own pods, allowed to reach sandbox pods under Restricted isolation")
	fs.DurationVar(&c.CNIProbeInterval, "cni-probe-interval",
		envOrDuration(getenv, "CNI_PROBE_INTERVAL", 5*time.Minute),
		"how often the CNI enforcement probe runs a pass")

	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parsing flags: %w", err)
	}

	return c, nil
}

// Validate returns an error naming the first invalid field, or nil if c is
// well-formed.
func (c Config) Validate() error {
	if c.SlotCapacity < 1 {
		return fmt.Errorf("slot-capacity: must be >= 1, got %d", c.SlotCapacity)
	}
	if c.ClusterID == "" {
		return fmt.Errorf("cluster-id: must not be empty")
	}
	if !dns1123LabelRE.MatchString(c.ClusterID) {
		return fmt.Errorf("cluster-id: %q is not a valid DNS-1123 label (lowercase alphanumeric and '-')", c.ClusterID)
	}
	if c.DefaultSandboxClass == "" {
		return fmt.Errorf("default-sandbox-class: must not be empty")
	}
	if c.ClassSecretNamespace == "" {
		return fmt.Errorf("class-secret-namespace: must not be empty")
	}
	if !dns1123LabelRE.MatchString(c.ClassSecretNamespace) {
		return fmt.Errorf("class-secret-namespace: %q is not a valid DNS-1123 label (lowercase alphanumeric and '-')", c.ClassSecretNamespace)
	}
	if c.SchedulerInterval < 100*time.Millisecond || c.SchedulerInterval > 5*time.Minute {
		return fmt.Errorf("scheduler-interval: must be between 100ms and 5m, got %s", c.SchedulerInterval)
	}
	if c.WarmCacheGCInterval < time.Second || c.WarmCacheGCInterval > time.Hour {
		return fmt.Errorf("warm-cache-gc-interval: must be between 1s and 1h, got %s", c.WarmCacheGCInterval)
	}
	if c.SidecarImage == "" {
		return fmt.Errorf("sidecar-image: must not be empty")
	}
	if c.OperatorIngressLabel == "" {
		return fmt.Errorf("operator-ingress-label: must not be empty")
	}
	if c.CNIProbeInterval < time.Second || c.CNIProbeInterval > time.Hour {
		return fmt.Errorf("cni-probe-interval: must be between 1s and 1h, got %s", c.CNIProbeInterval)
	}
	return nil
}

// classSecretNamespaceDefault resolves, in order: the prefixed env override,
// the raw POD_NAMESPACE (downward API convention, intentionally unprefixed),
// then a fixed fallback matching config/default/kustomization.yaml's namespace.
func classSecretNamespaceDefault(getenv func(string) string) string {
	if v := getenv(envPrefix + "CLASS_SECRET_NAMESPACE"); v != "" {
		return v
	}
	if v := getenv("POD_NAMESPACE"); v != "" {
		return v
	}
	return "ai-sandbox-operator-system"
}

func envOr(getenv func(string) string, name, def string) string {
	if v := getenv(envPrefix + name); v != "" {
		return v
	}
	return def
}

func envOrInt(getenv func(string) string, name string, def int) int {
	v := getenv(envPrefix + name)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err != nil {
		return def
	}
	return n
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

func envOrBool(getenv func(string) string, name string, def bool) bool {
	v := strings.TrimSpace(getenv(envPrefix + name))
	switch strings.ToLower(v) {
	case "":
		return def
	case "1", "t", "true", "yes":
		return true
	case "0", "f", "false", "no":
		return false
	default:
		return def
	}
}
