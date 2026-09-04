package config

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Built-in defaults, applied when neither a flag nor an environment variable
// supplies a value. GITHUB_REPO and AGENT_TOKEN are absent on purpose: they
// are required, and a default AGENT_TOKEN would be a hardcoded credential.
//
// The service URLs mirror ai-sandbox/docker-compose.yaml's claude service
// exactly, and the network names mirror the plan's resource-naming section,
// so a stock docker-operator stack needs no overrides at all.
const (
	defaultMaxAgents = 5

	defaultListenAddr  = ":8080"
	defaultStateDBPath = "/var/lib/docker-operator/state.db"
	defaultAgentImage  = "ghcr.io/psenna/ai-sandbox-docker-operator-agent:dev"

	defaultProxynetName = "docker-operator-proxynet"
	defaultDbnetName    = "docker-operator-dbnet"

	defaultGitProxyURL       = "http://git-proxy:8080"
	defaultGitProxyBrokerURL = "http://git-proxy:8090"

	defaultDependaproxyURL        = "http://dependaproxy:8080/npm"
	defaultDependaproxyPyPIURL    = "http://dependaproxy:8080/pypi"
	defaultDependaproxyGoproxyURL = "http://dependaproxy:8080/goproxy"

	defaultDockerRuntime = "sysbox-runc"

	defaultOllamaURL             = "http://ollama:11434"
	defaultAnthropicAuth         = "ollama" // NOT defaultAnthropicAuthToken -- gosec G101 pattern-matches "token" in identifier names
	defaultAgentModel            = "glm-5.2:cloud"
	defaultAgentFastModel        = "deepseek-v4-flash:0731-cloud"
	defaultDependaproxyContainer = "docker-operator-dependaproxy"
)

// redacted is what every stringification path of Secret emits in place of
// the real value.
const redacted = "[REDACTED]"

// Secret is a string that structurally refuses to print its own value.
// Every stringification path -- fmt's %v/%s/%q/%#v/%+v, encoding/json,
// encoding/text and log/slog's LogValuer -- is overridden to emit
// "[REDACTED]". Reveal is the only way to recover the real value.
//
// This mirrors operator/internal/storage.Secret, with one adaptation:
// LogValue (log/slog) replaces MarshalLog (logr), because docker-operator
// logs through the standard library and carries no logr dependency.
type Secret string

// String implements fmt.Stringer, covering %v, %s and %q.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer, covering %#v.
func (Secret) GoString() string { return redacted }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue implements slog.LogValuer, so a Secret passed as a structured
// logging value is redacted even by a handler that goes through neither fmt
// nor encoding/json.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Reveal returns the real value. The only sanctioned call sites are
// internal/agent's container-env templating and this package's own tests.
func (s Secret) Reveal() string { return string(s) }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s == "" }

// Config holds the docker-operator's runtime configuration, sourced from CLI
// flags with environment-variable fallbacks (flag > env > built-in default).
//
// It is a plain value: comparable, safe to copy, and holding no live
// resources. Building the per-agent container environment out of the
// passthrough fields below is internal/agent's job (issue #65), not this
// package's.
type Config struct {
	// MaxAgents is the hard cap on concurrently existing agents, enforced
	// synchronously by internal/agent.Create's reserve-under-capacity path
	// (409 over cap, no queueing). Must be >= 1; there is deliberately no
	// upper bound, matching operator's SlotCapacity -- any ceiling would be
	// a guess about the host's size.
	MaxAgents int

	// ListenAddr is the host:port the HTTP API, the WebSocket terminal
	// bridge and the embedded web UI all bind to. V1 has no auth, so this
	// should stay bound to a local interface on a shared host.
	ListenAddr string

	// StateDBPath is the BoltDB file holding agent records (issue #64).
	// The default is an absolute path under /var/lib because the operator
	// normally runs containerised with a named volume mounted there, where
	// the process's working directory is not a stable anchor. This package
	// validates the path's shape only; internal/store creates the file and
	// its parent directory.
	StateDBPath string

	// AgentImage is the image reference for agent containers -- the variant
	// built by docker-operator/agent/Dockerfile (issue #67), which adds tmux
	// and trims the K8s-operator-only skills.
	AgentImage string

	// ProxynetName is the shared network carrying the singleton ollama,
	// git-proxy and dependaproxy services, which every agent container
	// joins.
	ProxynetName string

	// DbnetName is the shared network carrying dependaproxy's postgres. It
	// is deliberately separate from proxynet -- that separation is the only
	// thing keeping the trust-anchor database unreachable from agents -- so
	// Validate rejects a configuration where the two names are equal.
	DbnetName string

	// GithubRepo is the owner/repo.git every agent clones through git-proxy.
	// REQUIRED: repo-specific, so no default can be correct. V1 shares one
	// repository across all agents (plan decision 3).
	GithubRepo string

	// AgentToken is the Bearer every agent presents to git-proxy. It is NOT
	// a GitHub PAT -- git-proxy consumes it for authentication only and
	// never forwards it. REQUIRED, and a Secret: it must never reach a log
	// line or an error message.
	AgentToken Secret

	// GitProxyURL is git-proxy's git-protocol endpoint, templated into each
	// agent's environment as GIT_PROXY_URL.
	GitProxyURL string

	// GitProxyBrokerURL is git-proxy's agent-facing broker REST endpoint
	// (PRs, CI status, issues), templated in as GIT_PROXY_BROKER_URL.
	GitProxyBrokerURL string

	// DependaproxyURL is dependaproxy's npm endpoint, templated in as
	// DEPENDAPROXY_URL. entrypoint.sh derives .npmrc from it.
	DependaproxyURL string

	// DependaproxyPyPIURL is dependaproxy's PyPI endpoint, templated in as
	// DEPENDAPROXY_PYPI_URL. entrypoint.sh derives pip.env from it.
	DependaproxyPyPIURL string

	// DependaproxyGoproxyURL is dependaproxy's Go module endpoint, templated
	// in as DEPENDAPROXY_GOPROXY_URL. entrypoint.sh derives go.env from it.
	DependaproxyGoproxyURL string

	// DockerRuntime is the container runtime for each agent's Docker-in-
	// Docker sidecar (issues #65/#69). The default, sysbox-runc, is what
	// makes an unprivileged inner daemon possible; overriding it to runc
	// would require --privileged and is not a supported configuration.
	DockerRuntime string

	// OllamaURL is the shared Ollama daemon's Anthropic-compatible endpoint,
	// templated into each agent as ANTHROPIC_BASE_URL. Empty is an explicit
	// escape hatch: omit the whole Ollama/model-routing block and let Claude
	// Code talk to the real Anthropic API using only AnthropicAPIKey.
	OllamaURL string

	// AnthropicAuthToken is the fixed placeholder token Claude Code sends to
	// the local Ollama daemon (not a real secret, but kept as a Secret for
	// consistency and because it's templated the same way AgentToken is).
	// Only used when OllamaURL is non-empty.
	AnthropicAuthToken Secret

	// AnthropicAPIKey is passed through as ANTHROPIC_API_KEY. Empty (the
	// default) is what stops Claude Code falling back to the Anthropic cloud
	// when a local backend (OllamaURL) is configured.
	AnthropicAPIKey Secret

	// AgentModel is the model every agent's default and "opus" tier resolves
	// to when OllamaURL is set.
	AgentModel string

	// AgentFastModel is the model every agent's "sonnet" and "haiku" tiers
	// resolve to when OllamaURL is set. Without mapping every tier, Task/
	// Explore subagents fail with "model may not exist" against a
	// non-Anthropic backend -- a documented gotcha in the root README.
	AgentFastModel string

	// DependaproxyContainer is the name of the shared DependaProxy container
	// the create flow connects to each new agent's private dinernet.
	DependaproxyContainer string
}

// Load parses args (typically os.Args[1:]) into a Config, falling back to
// the environment (via getenv) and then to built-in defaults for any flag
// not explicitly set on the command line.
//
// Load performs no validation beyond parsing: call Validate on the result.
// The returned error wraps flag's own, so a caller can still detect
// errors.Is(err, flag.ErrHelp) and exit 0 for -h.
func Load(args []string, getenv func(string) string) (Config, error) {
	var c Config

	// flag.StringVar cannot target a Secret, so the token is parsed into a
	// plain string and converted after Parse.
	var agentToken string
	var anthropicAuthToken string
	var anthropicAPIKey string

	maxAgents, err := envInt(getenv, "MAX_AGENTS", defaultMaxAgents)
	if err != nil {
		return Config{}, err
	}

	fs := flag.NewFlagSet("docker-operator", flag.ContinueOnError)

	fs.IntVar(&c.MaxAgents, "max-agents",
		maxAgents,
		"maximum number of agents that may exist concurrently; creates over the cap are rejected (env MAX_AGENTS)")
	fs.StringVar(&c.ListenAddr, "listen-addr",
		envOr(getenv, "LISTEN_ADDR", defaultListenAddr),
		"host:port the HTTP API, WebSocket terminal bridge and web UI bind to (env LISTEN_ADDR)")
	fs.StringVar(&c.StateDBPath, "state-db-path",
		envOr(getenv, "STATE_DB_PATH", defaultStateDBPath),
		"path to the BoltDB file holding agent records (env STATE_DB_PATH)")
	fs.StringVar(&c.AgentImage, "agent-image",
		envOr(getenv, "AGENT_IMAGE", defaultAgentImage),
		"container image for agent containers (env AGENT_IMAGE)")
	fs.StringVar(&c.ProxynetName, "proxynet-name",
		envOr(getenv, "PROXYNET_NAME", defaultProxynetName),
		"name of the shared network carrying ollama, git-proxy and dependaproxy (env PROXYNET_NAME)")
	fs.StringVar(&c.DbnetName, "dbnet-name",
		envOr(getenv, "DBNET_NAME", defaultDbnetName),
		"name of the shared network carrying dependaproxy's postgres; must differ from proxynet-name (env DBNET_NAME)")
	fs.StringVar(&c.GithubRepo, "github-repo",
		envOr(getenv, "GITHUB_REPO", ""),
		"required: owner/repo.git every agent clones through git-proxy (env GITHUB_REPO)")
	fs.StringVar(&agentToken, "agent-token",
		envOr(getenv, "AGENT_TOKEN", ""),
		"required: the Bearer every agent presents to git-proxy, not a GitHub PAT. Prefer the AGENT_TOKEN environment variable: flag values are visible to any user who can read the process table (env AGENT_TOKEN)")
	fs.StringVar(&c.GitProxyURL, "git-proxy-url",
		envOr(getenv, "GIT_PROXY_URL", defaultGitProxyURL),
		"git-proxy git-protocol endpoint templated into each agent (env GIT_PROXY_URL)")
	fs.StringVar(&c.GitProxyBrokerURL, "git-proxy-broker-url",
		envOr(getenv, "GIT_PROXY_BROKER_URL", defaultGitProxyBrokerURL),
		"git-proxy broker REST endpoint templated into each agent (env GIT_PROXY_BROKER_URL)")
	fs.StringVar(&c.DependaproxyURL, "dependaproxy-url",
		envOr(getenv, "DEPENDAPROXY_URL", defaultDependaproxyURL),
		"dependaproxy npm endpoint templated into each agent (env DEPENDAPROXY_URL)")
	fs.StringVar(&c.DependaproxyPyPIURL, "dependaproxy-pypi-url",
		envOr(getenv, "DEPENDAPROXY_PYPI_URL", defaultDependaproxyPyPIURL),
		"dependaproxy PyPI endpoint templated into each agent (env DEPENDAPROXY_PYPI_URL)")
	fs.StringVar(&c.DependaproxyGoproxyURL, "dependaproxy-goproxy-url",
		envOr(getenv, "DEPENDAPROXY_GOPROXY_URL", defaultDependaproxyGoproxyURL),
		"dependaproxy Go module endpoint templated into each agent (env DEPENDAPROXY_GOPROXY_URL)")
	fs.StringVar(&c.DockerRuntime, "docker-runtime",
		envOr(getenv, "DOCKER_RUNTIME", defaultDockerRuntime),
		"container runtime for each agent's Docker-in-Docker sidecar (env DOCKER_RUNTIME)")
	fs.StringVar(&c.OllamaURL, "ollama-url",
		envOr(getenv, "OLLAMA_URL", defaultOllamaURL),
		"shared Ollama daemon's Anthropic-compatible endpoint, templated into each agent as ANTHROPIC_BASE_URL; empty omits the whole model-routing block (env OLLAMA_URL)")
	fs.StringVar(&anthropicAuthToken, "anthropic-auth-token",
		envOr(getenv, "ANTHROPIC_AUTH_TOKEN", defaultAnthropicAuth),
		"placeholder token Claude Code sends to the local Ollama daemon; only used when ollama-url is non-empty (env ANTHROPIC_AUTH_TOKEN)")
	fs.StringVar(&anthropicAPIKey, "anthropic-api-key",
		envOr(getenv, "ANTHROPIC_API_KEY", ""),
		"passed through as ANTHROPIC_API_KEY; leave empty when ollama-url is set, so Claude Code cannot fall back to the Anthropic cloud (env ANTHROPIC_API_KEY)")
	fs.StringVar(&c.AgentModel, "agent-model",
		envOr(getenv, "AGENT_MODEL", defaultAgentModel),
		"model every agent's default and \"opus\" tier resolves to when ollama-url is set (env AGENT_MODEL)")
	fs.StringVar(&c.AgentFastModel, "agent-fast-model",
		envOr(getenv, "AGENT_FAST_MODEL", defaultAgentFastModel),
		"model every agent's \"sonnet\" and \"haiku\" tiers resolve to when ollama-url is set (env AGENT_FAST_MODEL)")
	fs.StringVar(&c.DependaproxyContainer, "dependaproxy-container",
		envOr(getenv, "DEPENDAPROXY_CONTAINER", defaultDependaproxyContainer),
		"name of the shared DependaProxy container the create flow connects to each new agent's private dinernet (env DEPENDAPROXY_CONTAINER)")

	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parsing flags: %w", err)
	}

	c.AgentToken = Secret(agentToken)
	c.AnthropicAuthToken = Secret(anthropicAuthToken)
	c.AnthropicAPIKey = Secret(anthropicAPIKey)

	return c, nil
}

// Validate returns an error naming the first invalid field, or nil if c is
// well-formed. Errors are prefixed with the flag name, and never contain the
// value of AgentToken.
//
// Validate checks the shape of the configuration only. Nothing here dials a
// Docker daemon, a git-proxy or a dependaproxy: reachability is
// internal/dockerclient's startup Ping (#63) and the agent create flow's
// concern, not config's.
func (c Config) Validate() error {
	if err := c.validateRequired(); err != nil {
		return err
	}
	if err := c.validateLimitsAndPaths(); err != nil {
		return err
	}
	if err := c.validateNetworks(); err != nil {
		return err
	}
	if err := c.validateURLs(); err != nil {
		return err
	}
	return c.validateModelRouting()
}

// validateRequired covers the two values that have no default and must be
// supplied by the operator's environment.
func (c Config) validateRequired() error {
	if c.GithubRepo == "" {
		return fmt.Errorf("github-repo: must not be empty; set the GITHUB_REPO environment variable or -github-repo to the owner/repo.git every agent clones")
	}
	if c.AgentToken.IsZero() {
		return fmt.Errorf("agent-token: must not be empty; set the AGENT_TOKEN environment variable to the Bearer every agent presents to git-proxy")
	}
	return nil
}

func (c Config) validateLimitsAndPaths() error {
	if c.MaxAgents < 1 {
		return fmt.Errorf("max-agents: must be >= 1, got %d", c.MaxAgents)
	}
	if err := validateHostPort("listen-addr", c.ListenAddr); err != nil {
		return err
	}
	if c.StateDBPath == "" {
		return fmt.Errorf("state-db-path: must not be empty")
	}
	if strings.HasSuffix(c.StateDBPath, "/") {
		return fmt.Errorf("state-db-path: %q ends in a separator, want a file path not a directory", c.StateDBPath)
	}
	if c.AgentImage == "" {
		return fmt.Errorf("agent-image: must not be empty")
	}
	if c.DockerRuntime == "" {
		return fmt.Errorf("docker-runtime: must not be empty")
	}
	if c.DependaproxyContainer == "" {
		return fmt.Errorf("dependaproxy-container: must not be empty")
	}
	return nil
}

func (c Config) validateNetworks() error {
	if err := validateDockerName("proxynet-name", c.ProxynetName); err != nil {
		return err
	}
	if err := validateDockerName("dbnet-name", c.DbnetName); err != nil {
		return err
	}
	if c.ProxynetName == c.DbnetName {
		return fmt.Errorf("proxynet-name and dbnet-name: must differ, both are %q; dependaproxy's postgres is reachable only from dbnet and collapsing the two networks would remove that isolation", c.ProxynetName)
	}
	return nil
}

func (c Config) validateURLs() error {
	for _, f := range []struct{ field, value string }{
		{"git-proxy-url", c.GitProxyURL},
		{"git-proxy-broker-url", c.GitProxyBrokerURL},
		{"dependaproxy-url", c.DependaproxyURL},
		{"dependaproxy-pypi-url", c.DependaproxyPyPIURL},
		{"dependaproxy-goproxy-url", c.DependaproxyGoproxyURL},
	} {
		if err := validateHTTPURL(f.field, f.value); err != nil {
			return err
		}
	}
	return nil
}

// validateModelRouting covers the Ollama/Anthropic model-routing block.
// OllamaURL="" is the escape hatch (real Anthropic API via AnthropicAPIKey
// alone), so it is the only field validated unconditionally; AgentModel and
// AgentFastModel only matter once a local backend is actually configured.
func (c Config) validateModelRouting() error {
	if c.OllamaURL == "" {
		return nil
	}
	if err := validateHTTPURL("ollama-url", c.OllamaURL); err != nil {
		return err
	}
	if c.AgentModel == "" {
		return fmt.Errorf("agent-model: must not be empty when ollama-url is set")
	}
	if c.AgentFastModel == "" {
		return fmt.Errorf("agent-fast-model: must not be empty when ollama-url is set")
	}
	return nil
}

// dockerNameRE matches the character set Docker accepts for a network name.
var dockerNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func validateDockerName(field, name string) error {
	if name == "" {
		return fmt.Errorf("%s: must not be empty", field)
	}
	if !dockerNameRE.MatchString(name) {
		return fmt.Errorf("%s: %q is not a valid Docker network name (must match %s)", field, name, dockerNameRE)
	}
	return nil
}

func validateHostPort(field, addr string) error {
	if addr == "" {
		return fmt.Errorf("%s: must not be empty", field)
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s: %q is not a valid host:port address: %w", field, addr, err)
	}
	if port == "" {
		return fmt.Errorf("%s: %q is missing a port", field, addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("%s: %q has a non-numeric port %q, want a number in 0-65535", field, addr, port)
	}
	if n < 0 || n > 65535 {
		return fmt.Errorf("%s: port %d is out of range 0-65535", field, n)
	}
	return nil
}

func validateHTTPURL(field, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s: must not be empty", field)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %q is not a valid URL: %w", field, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: %q must use the http or https scheme, got %q", field, raw, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: %q has no host", field, raw)
	}
	return nil
}

// envOr returns the environment value for name, or def when it is unset or
// empty. Unlike operator/internal/config, names are not prefixed -- see
// doc.go for why.
func envOr(getenv func(string) string, name, def string) string {
	if v := getenv(name); v != "" {
		return v
	}
	return def
}

// envInt returns the integer environment value for name, or def when it is
// unset or blank.
//
// This deliberately diverges from operator/internal/config's envOrInt, which
// silently falls back to the default on an unparseable value: MAX_AGENTS is
// a hard cap on host resource consumption rather than a tuning knob, and
// silently reinterpreting a typo as the default is a lie the operator has no
// way to surface.
func envInt(getenv func(string) string, name string, def int) (int, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a valid integer: %w", name, v, err)
	}
	return n, nil
}
