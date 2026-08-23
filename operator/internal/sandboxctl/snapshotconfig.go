package sandboxctl

import (
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// SnapshotConfig is how the sidecar learns its freeze-snapshot backend
// configuration. The sidecar has NO RBAC to read the cluster-scoped
// SandboxClass and does not mount the <env>-config ConfigMap (see
// internal/render/pod.go's sidecarContainer: it mounts only the token
// volume, workspace, and agent-home), so every value here is projected via
// CLI flags on the sidecar container -- visible/reviewable in the golden
// pod files. See internal/render/pod.go's sidecarSnapshotArgs for the
// operator-side flag construction.
type SnapshotConfig struct {
	// Engine selects the EngineTeardown implementation. Rendered from
	// class.Spec.Engine.Type.
	Engine string

	// EngineEndpoint is the pod-loopback engine API endpoint the
	// EngineTeardown dials (#24: tcp://127.0.0.1:2375). Rendered by
	// internal/render/pod.go's sidecarSnapshotArgs from the SAME constant the
	// agent's DOCKER_HOST comes from (render.PodmanDockerHost). Empty for
	// engine: none.
	EngineEndpoint string

	// ClusterID is storage.Identity.ClusterID. Rendered from
	// Reconciler.ClusterID.
	ClusterID string
	// SpecHash is "sha256:<hex>", computed operator-side via
	// storage.SpecHash(&class.Spec, &env.Spec).
	SpecHash string
	// AgentImage is required by storage.Manifest.Validate.
	AgentImage string

	// WorkspacePath and AgentHomePath are the mount paths archived. An
	// empty AgentHomePath means "skip that archive entirely" -- the
	// recovery-Job case (#28's freeze-once mode), where the agent home is
	// an emptyDir that no longer exists.
	WorkspacePath string
	AgentHomePath string

	// Backend selects the snapshot storage backend: "" (freeze not
	// configured), "s3", or "pvc". "pvc" is accepted at config time but
	// fails closed at freeze time (see snapshot.go), naming gap G5.
	Backend string

	S3Endpoint       string
	S3Bucket         string
	S3Region         string
	S3Prefix         string
	S3ForcePathStyle bool

	// CredentialsDir is the fixed mount path of the Secret volume the
	// operator projects into this container only.
	CredentialsDir string

	// QuiesceDelay is the bounded settle window after engine teardown,
	// before archiving begins -- the sidecar has no handle on the agent
	// process and no pods RBAC, so this is the only quiesce mechanism.
	QuiesceDelay time.Duration

	// SnapshotRetries bounds retrySnapshotStep's retry loop.
	SnapshotRetries int

	// SnapshotStepTimeout bounds each individual archive/upload attempt
	// inside retrySnapshotStep -- see that function's doc comment for why
	// this exists (the S3 client's http.Client has no built-in Timeout).
	SnapshotStepTimeout time.Duration
}

const (
	defaultSnapshotWorkspacePath = "/workspace"                 // must match render.WorkspaceMountPath
	defaultSnapshotAgentHomePath = "/home/node/.claude-sandbox" // must match render.AgentHomePath
	// defaultSnapshotCredentialsDir is a mount PATH, not a credential value.
	defaultSnapshotCredentialsDir = "/var/run/secrets/ai-sandbox/snapshot" //nolint:gosec // G101: a mount path, not a credential value -- matches render.SnapshotCredentialsMountPath's own nolint
	defaultQuiesceDelay           = 2 * time.Second
	defaultSnapshotRetries        = 4
	defaultSnapshotStepTimeout    = 2 * time.Minute
)

// registerSnapshotFlags registers SnapshotConfig's flags on fs, with
// environment-variable fallbacks matching config.go's envOr/envOrDuration
// convention.
func registerSnapshotFlags(fs *flag.FlagSet, c *SnapshotConfig, getenv func(string) string) {
	fs.StringVar(&c.Engine, "engine", envOr(getenv, "ENGINE", "none"),
		"container engine type this sandbox uses; selects the EngineTeardown implementation")
	fs.StringVar(&c.EngineEndpoint, "engine-endpoint", envOr(getenv, "ENGINE_ENDPOINT", ""),
		"pod-loopback engine API endpoint the EngineTeardown dials (tcp://host:port)")
	fs.StringVar(&c.ClusterID, "cluster-id", envOr(getenv, "CLUSTER_ID", ""),
		"cluster identifier, used as a storage path segment")
	fs.StringVar(&c.SpecHash, "spec-hash", envOr(getenv, "SPEC_HASH", ""),
		"sha256:<hex> of the class+env specs, computed operator-side")
	fs.StringVar(&c.AgentImage, "agent-image", envOr(getenv, "AGENT_IMAGE", ""),
		"agent container image this snapshot is produced under")
	fs.StringVar(&c.WorkspacePath, "workspace-path", envOr(getenv, "WORKSPACE_PATH", defaultSnapshotWorkspacePath),
		"path to the workspace root archived on freeze")
	fs.StringVar(&c.AgentHomePath, "agent-home-path", envOr(getenv, "AGENT_HOME_PATH", defaultSnapshotAgentHomePath),
		"path to the agent home archived on freeze; empty skips this archive (the recovery-Job case)")
	fs.StringVar(&c.Backend, "snapshot-backend", envOr(getenv, "SNAPSHOT_BACKEND", ""),
		`snapshot storage backend: "" (disabled), "s3", or "pvc" (fails closed at freeze time)`)
	fs.StringVar(&c.S3Endpoint, "s3-endpoint", envOr(getenv, "S3_ENDPOINT", ""), "s3 endpoint URL")
	fs.StringVar(&c.S3Bucket, "s3-bucket", envOr(getenv, "S3_BUCKET", ""), "s3 bucket")
	fs.StringVar(&c.S3Region, "s3-region", envOr(getenv, "S3_REGION", ""), "s3 region")
	fs.StringVar(&c.S3Prefix, "s3-prefix", envOr(getenv, "S3_PREFIX", ""), "s3 key prefix")
	fs.BoolVar(&c.S3ForcePathStyle, "s3-force-path-style", envOrBool(getenv, "S3_FORCE_PATH_STYLE", true), "use path-style s3 addressing")
	fs.StringVar(&c.CredentialsDir, "snapshot-credentials-dir", envOr(getenv, "SNAPSHOT_CREDENTIALS_DIR", defaultSnapshotCredentialsDir),
		"directory containing the projected s3 credentials Secret")
	fs.DurationVar(&c.QuiesceDelay, "quiesce-delay", envOrDuration(getenv, "QUIESCE_DELAY", defaultQuiesceDelay),
		"bounded settle window after engine teardown, before archiving begins")
	fs.IntVar(&c.SnapshotRetries, "snapshot-retries", envOrInt(getenv, "SNAPSHOT_RETRIES", defaultSnapshotRetries),
		"number of retries for each archive/upload step")
	fs.DurationVar(&c.SnapshotStepTimeout, "snapshot-step-timeout", envOrDuration(getenv, "SNAPSHOT_STEP_TIMEOUT", defaultSnapshotStepTimeout),
		"per-attempt timeout for each archive/upload step, so a wedged backend call can't stall the retry budget indefinitely")
}

// Validate checks SnapshotConfig for internal consistency. An empty Backend
// means freeze is simply not configured for this sidecar (the noop hook is
// selected) -- not an error.
func (c SnapshotConfig) Validate() error {
	if c.Engine == "rootless-podman" && c.EngineEndpoint == "" {
		return fmt.Errorf("engine-endpoint: required when engine is %q", c.Engine)
	}
	switch c.Backend {
	case "":
		return nil
	case "pvc":
		// Accepted at config time; snapshot.go fails closed at freeze time
		// with SnapshotReasonBackendUnsupported (gap G5: the backend PVC is
		// not mounted into the environment pod).
		return nil
	case "s3":
		if c.S3Endpoint == "" {
			return fmt.Errorf("s3-endpoint: required when snapshot-backend=s3")
		}
		u, err := url.Parse(c.S3Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("s3-endpoint: %q must be a valid http(s):// URL", c.S3Endpoint)
		}
		if c.S3Bucket == "" {
			return fmt.Errorf("s3-bucket: required when snapshot-backend=s3")
		}
		if c.SpecHash == "" {
			return fmt.Errorf("spec-hash: required when snapshot-backend=s3")
		}
		if c.AgentImage == "" {
			return fmt.Errorf("agent-image: required when snapshot-backend=s3")
		}
		if c.CredentialsDir == "" {
			return fmt.Errorf("snapshot-credentials-dir: required when snapshot-backend=s3")
		}
		if c.WorkspacePath == "" {
			return fmt.Errorf("workspace-path: must not be empty")
		}
		return nil
	default:
		return fmt.Errorf("snapshot-backend: unknown value %q; must be \"\", \"s3\", or \"pvc\"", c.Backend)
	}
}

// BuildBackend constructs the storage.Backend cfg configures. Only ever
// called when cfg.Backend == "s3": the "pvc" case fails closed inside
// snapshot.go, and "" selects the noop FreezeHook before BuildBackend is
// ever invoked (see run.go).
func BuildBackend(cfg SnapshotConfig) (storage.Backend, error) {
	if cfg.Backend != "s3" {
		return nil, fmt.Errorf("sandboxctl: BuildBackend called with unsupported backend %q", cfg.Backend)
	}
	creds, err := ReadCredentialsDir(cfg.CredentialsDir)
	if err != nil {
		return nil, fmt.Errorf("sandboxctl: reading snapshot credentials: %w", err)
	}
	s3cfg := storage.S3Config{
		Endpoint:       cfg.S3Endpoint,
		Bucket:         cfg.S3Bucket,
		Region:         cfg.S3Region,
		ForcePathStyle: cfg.S3ForcePathStyle,
	}
	be, err := storage.NewS3(s3cfg, creds)
	if err != nil {
		return nil, fmt.Errorf("sandboxctl: building s3 backend: %w", err)
	}
	return be, nil
}

// snapshotLayout builds the storage.Layout for one environment.
func snapshotLayout(cfg SnapshotConfig, namespace, name, uid string) (storage.Layout, error) {
	return storage.NewLayout(cfg.S3Prefix, storage.Identity{
		ClusterID: cfg.ClusterID,
		Namespace: namespace,
		EnvName:   name,
		EnvUID:    uid,
	})
}

func envOrBool(getenv func(string) string, name string, def bool) bool {
	v := getenv(envPrefix + name)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envOrInt(getenv func(string) string, name string, def int) int {
	v := getenv(envPrefix + name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
