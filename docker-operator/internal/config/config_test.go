package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

func emptyEnv(string) string { return "" }

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// requiredEnv is a representative minimal deployment: AGENT_TOKEN (the one
// value with no default) plus a GITHUB_REPO -- optional now, but the common
// case. Everything else is expected to default.
func requiredEnv() map[string]string {
	return map[string]string{
		"GITHUB_REPO": "psenna/ai-sandbox.git",
		"AGENT_TOKEN": "agent-token-1",
	}
}

// envWith returns requiredEnv overlaid with extra. A "" value removes the
// key, so a case can drop a variable.
func envWith(extra map[string]string) func(string) string {
	m := requiredEnv()
	for k, v := range extra {
		if v == "" {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return envFrom(m)
}

// TestLoad_DefaultsWithOnlyRequiredEnv pins every default. The literals are
// spelled out rather than referencing the package constants on purpose: a
// change to any default must break this test.
func TestLoad_DefaultsWithOnlyRequiredEnv(t *testing.T) {
	c, err := Load(nil, envFrom(requiredEnv()))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	// Checked separately so a mismatch is legible: both sides of the struct
	// comparison below render the token as [REDACTED].
	if got := c.AgentToken.Reveal(); got != "agent-token-1" {
		t.Fatalf("AgentToken = %q, want the value from the environment", got)
	}

	want := Config{
		MaxAgents:              5,
		ListenAddr:             ":8080",
		StateDBPath:            "/var/lib/docker-operator/state.db",
		AgentImage:             "ghcr.io/psenna/ai-sandbox-docker-operator-agent:dev",
		ProxynetName:           "docker-operator-proxynet",
		DbnetName:              "docker-operator-dbnet",
		GithubRepo:             "psenna/ai-sandbox.git",
		AgentToken:             Secret("agent-token-1"),
		APIToken:               Secret(""),
		GitProxyURL:            "http://git-proxy:8080",
		GitProxyBrokerURL:      "http://git-proxy:8090",
		DependaproxyURL:        "http://dependaproxy:8080/npm",
		DependaproxyPyPIURL:    "http://dependaproxy:8080/pypi",
		DependaproxyGoproxyURL: "http://dependaproxy:8080/goproxy",
		DockerRuntime:          "sysbox-runc",
		DefaultBackend:         "ollama",
		OllamaURL:              "http://ollama:11434",
		AnthropicAuthToken:     Secret("ollama"),
		AnthropicAPIKey:        Secret(""),
		AgentModel:             "glm-5.3:cloud",
		AgentFastModel:         "glm-5.3-flash:cloud",
		DependaproxyContainer:  "docker-operator-dependaproxy",
	}
	if c != want {
		t.Fatalf("Load defaults = %+v, want %+v", c, want)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() on defaults: %v", err)
	}
}

// fieldCases enumerates every configurable field exactly once, so the three
// precedence tests below cover every env and flag override path in the
// package. Adding a field to Config without adding it here leaves an
// override path untested.
var fieldCases = []struct {
	name      string
	env       string
	flag      string
	envValue  string
	flagValue string
	get       func(Config) string
}{
	{"MaxAgents", "MAX_AGENTS", "max-agents", "7", "9",
		func(c Config) string { return strconv.Itoa(c.MaxAgents) }},
	{"ListenAddr", "LISTEN_ADDR", "listen-addr", "127.0.0.1:9000", "127.0.0.1:9100",
		func(c Config) string { return c.ListenAddr }},
	{"StateDBPath", "STATE_DB_PATH", "state-db-path", "/env/state.db", "/flag/state.db",
		func(c Config) string { return c.StateDBPath }},
	{"AgentImage", "AGENT_IMAGE", "agent-image", "example.com/env:v1", "example.com/flag:v1",
		func(c Config) string { return c.AgentImage }},
	{"ProxynetName", "PROXYNET_NAME", "proxynet-name", "env-proxynet", "flag-proxynet",
		func(c Config) string { return c.ProxynetName }},
	{"DbnetName", "DBNET_NAME", "dbnet-name", "env-dbnet", "flag-dbnet",
		func(c Config) string { return c.DbnetName }},
	{"GithubRepo", "GITHUB_REPO", "github-repo", "env/repo.git", "flag/repo.git",
		func(c Config) string { return c.GithubRepo }},
	{"AgentToken", "AGENT_TOKEN", "agent-token", "env-bearer", "flag-bearer",
		func(c Config) string { return c.AgentToken.Reveal() }},
	{"APIToken", "OPERATOR_API_TOKEN", "api-token", "env-api-token", "flag-api-token",
		func(c Config) string { return c.APIToken.Reveal() }},
	{"GitProxyURL", "GIT_PROXY_URL", "git-proxy-url", "http://env-proxy:1", "http://flag-proxy:1",
		func(c Config) string { return c.GitProxyURL }},
	{"GitProxyBrokerURL", "GIT_PROXY_BROKER_URL", "git-proxy-broker-url", "http://env-broker:1", "http://flag-broker:1",
		func(c Config) string { return c.GitProxyBrokerURL }},
	{"DependaproxyURL", "DEPENDAPROXY_URL", "dependaproxy-url", "http://env-dp:1/npm", "http://flag-dp:1/npm",
		func(c Config) string { return c.DependaproxyURL }},
	{"DependaproxyPyPIURL", "DEPENDAPROXY_PYPI_URL", "dependaproxy-pypi-url", "http://env-dp:1/pypi", "http://flag-dp:1/pypi",
		func(c Config) string { return c.DependaproxyPyPIURL }},
	{"DependaproxyGoproxyURL", "DEPENDAPROXY_GOPROXY_URL", "dependaproxy-goproxy-url", "http://env-dp:1/goproxy", "http://flag-dp:1/goproxy",
		func(c Config) string { return c.DependaproxyGoproxyURL }},
	{"DockerRuntime", "DOCKER_RUNTIME", "docker-runtime", "env-runc", "flag-runc",
		func(c Config) string { return c.DockerRuntime }},
	{"DefaultBackend", "DEFAULT_AGENT_BACKEND", "default-backend", "anthropic", "ollama",
		func(c Config) string { return c.DefaultBackend }},
	{"OllamaURL", "OLLAMA_URL", "ollama-url", "http://env-ollama:11434", "http://flag-ollama:11434",
		func(c Config) string { return c.OllamaURL }},
	{"AnthropicAuthToken", "ANTHROPIC_AUTH_TOKEN", "anthropic-auth-token", "env-auth", "flag-auth",
		func(c Config) string { return c.AnthropicAuthToken.Reveal() }},
	{"AnthropicAPIKey", "ANTHROPIC_API_KEY", "anthropic-api-key", "env-key", "flag-key",
		func(c Config) string { return c.AnthropicAPIKey.Reveal() }},
	{"AgentModel", "AGENT_MODEL", "agent-model", "env-model", "flag-model",
		func(c Config) string { return c.AgentModel }},
	{"AgentFastModel", "AGENT_FAST_MODEL", "agent-fast-model", "env-fast-model", "flag-fast-model",
		func(c Config) string { return c.AgentFastModel }},
	{"DependaproxyContainer", "DEPENDAPROXY_CONTAINER", "dependaproxy-container", "env-dependaproxy", "flag-dependaproxy",
		func(c Config) string { return c.DependaproxyContainer }},
}

func TestLoad_EnvOverride(t *testing.T) {
	for _, tc := range fieldCases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(nil, envWith(map[string]string{tc.env: tc.envValue}))
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			if got := tc.get(c); got != tc.envValue {
				t.Errorf("%s = %q, want %q from %s", tc.name, got, tc.envValue, tc.env)
			}
			if err := c.Validate(); err != nil {
				t.Errorf("Validate() after %s override: unexpected error: %v", tc.env, err)
			}
		})
	}
}

func TestLoad_FlagBeatsEnv(t *testing.T) {
	for _, tc := range fieldCases {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--" + tc.flag + "=" + tc.flagValue}
			c, err := Load(args, envWith(map[string]string{tc.env: tc.envValue}))
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			if got := tc.get(c); got != tc.flagValue {
				t.Errorf("%s = %q, want %q (flag must beat env)", tc.name, got, tc.flagValue)
			}
		})
	}
}

func TestLoad_MaxAgentsEnvParsing(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{"valid", "12", 12, false},
		{"surrounding whitespace tolerated", "  12  ", 12, false},
		{"blank falls back to default", "   ", 5, false},
		{"negative parses, rejected by Validate", "-3", -3, false},
		{"not a number", "twenty", 0, true},
		{"float", "5.5", 0, true},
		{"trailing junk", "5x", 0, true},
		{"overflows int64", "99999999999999999999999", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(nil, envWith(map[string]string{"MAX_AGENTS": tc.value}))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load with MAX_AGENTS=%q: expected error, got nil", tc.value)
				}
				if !strings.Contains(err.Error(), "MAX_AGENTS") {
					t.Errorf("Load error = %v, want it to name MAX_AGENTS", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load with MAX_AGENTS=%q: unexpected error: %v", tc.value, err)
			}
			if c.MaxAgents != tc.want {
				t.Errorf("MaxAgents = %d, want %d", c.MaxAgents, tc.want)
			}
		})
	}
}

// TestValidate_Errors covers every failure branch in Validate. Each case
// asserts a non-nil error naming the offending flag -- never a panic, and
// never a bare "invalid config".
func TestValidate_Errors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string // overlaid on requiredEnv; "" deletes the key
		args []string
		want string // substring the error must contain
	}{
		{name: "GITHUB_REPO is a full URL", env: map[string]string{"GITHUB_REPO": "https://github.com/psenna/ai-sandbox"}, want: "github-repo"},
		{name: "GITHUB_REPO has no owner", args: []string{"--github-repo=ai-sandbox.git"}, want: "github-repo"},
		{name: "GITHUB_REPO has a nested path", args: []string{"--github-repo=psenna/ai-sandbox/tree/main"}, want: "github-repo"},
		{name: "AGENT_TOKEN missing", env: map[string]string{"AGENT_TOKEN": ""}, want: "agent-token"},
		{name: "AGENT_TOKEN empty via flag", args: []string{"--agent-token="}, want: "agent-token"},

		{name: "max-agents zero", args: []string{"--max-agents=0"}, want: "max-agents"},
		{name: "max-agents negative", args: []string{"--max-agents=-3"}, want: "max-agents"},

		{name: "listen-addr empty", args: []string{"--listen-addr="}, want: "listen-addr"},
		{name: "listen-addr missing port", args: []string{"--listen-addr=8080"}, want: "listen-addr"},
		{name: "listen-addr blank port", args: []string{"--listen-addr=localhost:"}, want: "listen-addr"},
		{name: "listen-addr named port", args: []string{"--listen-addr=localhost:http"}, want: "listen-addr"},
		{name: "listen-addr port out of range", args: []string{"--listen-addr=:70000"}, want: "listen-addr"},

		{name: "state-db-path empty", args: []string{"--state-db-path="}, want: "state-db-path"},
		{name: "state-db-path is a directory", args: []string{"--state-db-path=/var/lib/docker-operator/"}, want: "state-db-path"},

		{name: "agent-image empty", args: []string{"--agent-image="}, want: "agent-image"},
		{name: "docker-runtime empty", args: []string{"--docker-runtime="}, want: "docker-runtime"},

		{name: "default-backend empty", args: []string{"--default-backend="}, want: "default-backend"},
		{name: "default-backend unknown", args: []string{"--default-backend=vertex"}, want: "default-backend"},

		{name: "proxynet-name empty", args: []string{"--proxynet-name="}, want: "proxynet-name"},
		{name: "proxynet-name has a space", args: []string{"--proxynet-name=bad name"}, want: "proxynet-name"},
		{name: "proxynet-name has a slash", args: []string{"--proxynet-name=a/b"}, want: "proxynet-name"},
		{name: "dbnet-name empty", args: []string{"--dbnet-name="}, want: "dbnet-name"},
		{name: "dbnet-name leading dash", args: []string{"--dbnet-name=-leading"}, want: "dbnet-name"},
		{name: "network names identical", args: []string{"--proxynet-name=shared", "--dbnet-name=shared"}, want: "must differ"},

		{name: "git-proxy-url empty", args: []string{"--git-proxy-url="}, want: "git-proxy-url"},
		{name: "git-proxy-url no scheme", args: []string{"--git-proxy-url=//git-proxy:8080"}, want: "git-proxy-url"},
		{name: "git-proxy-url wrong scheme", args: []string{"--git-proxy-url=ftp://git-proxy:8080"}, want: "git-proxy-url"},
		{name: "git-proxy-url no host", args: []string{"--git-proxy-url=http://"}, want: "git-proxy-url"},
		{name: "git-proxy-url unparseable", args: []string{"--git-proxy-url=http://[::1"}, want: "git-proxy-url"},
		{name: "git-proxy-broker-url empty", args: []string{"--git-proxy-broker-url="}, want: "git-proxy-broker-url"},
		{name: "dependaproxy-url wrong scheme", args: []string{"--dependaproxy-url=ftp://dependaproxy:8080/npm"}, want: "dependaproxy-url"},
		{name: "dependaproxy-pypi-url empty", args: []string{"--dependaproxy-pypi-url="}, want: "dependaproxy-pypi-url"},
		{name: "dependaproxy-goproxy-url no host", args: []string{"--dependaproxy-goproxy-url=http://"}, want: "dependaproxy-goproxy-url"},

		{name: "ollama-url wrong scheme", args: []string{"--ollama-url=ftp://ollama:11434"}, want: "ollama-url"},
		{name: "ollama-url no host", args: []string{"--ollama-url=http://"}, want: "ollama-url"},
		{name: "agent-model empty while ollama-url is set", args: []string{"--agent-model="}, want: "agent-model"},
		{name: "agent-fast-model empty while ollama-url is set", args: []string{"--agent-fast-model="}, want: "agent-fast-model"},
		{name: "dependaproxy-container empty", args: []string{"--dependaproxy-container="}, want: "dependaproxy-container"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(tc.args, envWith(tc.env))
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			err = c.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() error = %v, want it to mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "agent-token-1") {
				t.Errorf("Validate() error leaked the agent token: %v", err)
			}
		})
	}
}

func TestValidate_AcceptsBoundaryValues(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"max-agents at the lower bound", []string{"--max-agents=1"}},
		{"listen-addr with port 0", []string{"--listen-addr=:0"}},
		{"listen-addr with the maximum port", []string{"--listen-addr=:65535"}},
		{"listen-addr with an IPv6 host", []string{"--listen-addr=[::1]:8080"}},
		{"listen-addr with a bare colon port", []string{"--listen-addr=:8080"}},
		{"https service URL", []string{"--git-proxy-url=https://git-proxy.internal"}},
		{"relative state db path", []string{"--state-db-path=state.db"}},
		{"network name with dots and underscores", []string{"--proxynet-name=a.b_c-1"}},
		{"empty ollama-url is the escape hatch, even with agent-model/agent-fast-model empty too",
			[]string{"--ollama-url=", "--agent-model=", "--agent-fast-model="}},
		{"https ollama-url", []string{"--ollama-url=https://ollama.internal:11434"}},
		{"default-backend anthropic", []string{"--default-backend=anthropic"}},
		{"default-backend ollama", []string{"--default-backend=ollama"}},
		{"empty github-repo is allowed -- the agent boots as a bare terminal", []string{"--github-repo="}},
		{"github-repo without a .git suffix", []string{"--github-repo=psenna/ai-sandbox"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(tc.args, envFrom(requiredEnv()))
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			if err := c.Validate(); err != nil {
				t.Errorf("Validate() with %v: unexpected error: %v", tc.args, err)
			}
		})
	}
}

func TestValidGithubRepo(t *testing.T) {
	valid := []string{
		"psenna/ai-sandbox.git", "psenna/ai-sandbox", "a/b",
		"my-org/my_repo.v2", "Owner123/repo-name",
	}
	for _, s := range valid {
		if !ValidGithubRepo(s) {
			t.Errorf("ValidGithubRepo(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"", "no-slash", "psenna/ai-sandbox/tree/main",
		"https://github.com/psenna/ai-sandbox", "git@github.com:psenna/ai-sandbox.git",
		"psenna /ai-sandbox", "/leading", "trailing/", "owner/re po",
	}
	for _, s := range invalid {
		if ValidGithubRepo(s) {
			t.Errorf("ValidGithubRepo(%q) = true, want false", s)
		}
	}
}

// TestLoadValidate_NeverPanics is the acceptance criterion's "clear error,
// not a panic" stated directly: every variable gets every hostile value, and
// the only acceptable outcomes are a Config or an error.
func TestLoadValidate_NeverPanics(t *testing.T) {
	hostile := []string{
		"", " ", "\t", "\n", "\x00", "-1", "0", "!!!", "://", "%%%%",
		"http://[::1", ":", "::::", "abc", strings.Repeat("a", 4096),
	}
	names := []string{
		"MAX_AGENTS", "LISTEN_ADDR", "STATE_DB_PATH", "AGENT_IMAGE",
		"PROXYNET_NAME", "DBNET_NAME", "GITHUB_REPO", "AGENT_TOKEN",
		"OPERATOR_API_TOKEN",
		"GIT_PROXY_URL", "GIT_PROXY_BROKER_URL", "DEPENDAPROXY_URL",
		"DEPENDAPROXY_PYPI_URL", "DEPENDAPROXY_GOPROXY_URL", "DOCKER_RUNTIME",
		"OLLAMA_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY",
		"AGENT_MODEL", "AGENT_FAST_MODEL", "DEPENDAPROXY_CONTAINER",
	}

	for _, name := range names {
		for _, v := range hostile {
			t.Run(name+"="+strconv.Quote(v), func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("Load/Validate panicked with %s=%q: %v", name, v, r)
					}
				}()
				m := requiredEnv()
				m[name] = v
				c, err := Load(nil, envFrom(m))
				if err != nil {
					return // an error is a correct outcome; a panic is not
				}
				if err := c.Validate(); err != nil {
					return
				}
			})
		}
	}
}

func TestLoad_UnknownFlagErrors(t *testing.T) {
	if _, err := Load([]string{"--does-not-exist=1"}, emptyEnv); err == nil {
		t.Fatal("Load with an unknown flag: expected error, got nil")
	}
}

// TestLoad_HelpIsReportedAsErrHelp keeps `docker-operator -h` distinguishable
// from a real failure, so cmd/docker-operator can exit 0 for it.
func TestLoad_HelpIsReportedAsErrHelp(t *testing.T) {
	_, err := Load([]string{"-h"}, emptyEnv)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("Load(-h) error = %v, want it to wrap flag.ErrHelp", err)
	}
}

// TestAgentToken_NeverLeaksThroughStringification mirrors
// operator/internal/storage's credentials_test.go: every rendering path a
// log line or an error message could plausibly take must redact. Both Secret
// fields loaded from the environment (AgentToken and APIToken) are exercised.
func TestAgentToken_NeverLeaksThroughStringification(t *testing.T) {
	sentinel := "correct-horse-battery-staple"
	apiSentinel := "hunter2-operator-api-sentinel"

	c, err := Load(nil, envWith(map[string]string{"AGENT_TOKEN": sentinel, "OPERATOR_API_TOKEN": apiSentinel}))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := c.AgentToken.Reveal(); got != sentinel {
		t.Fatalf("Reveal() = %q, want the real value %q", got, sentinel)
	}
	if got := c.APIToken.Reveal(); got != apiSentinel {
		t.Fatalf("APIToken.Reveal() = %q, want the real value %q", got, apiSentinel)
	}

	jsonSecret, err := json.Marshal(c.AgentToken)
	if err != nil {
		t.Fatalf("json.Marshal(Secret): %v", err)
	}
	// G117 flags a *-Token/-Key struct field being marshaled; here that is the
	// property under test -- config.Secret.MarshalJSON redacts, and the
	// assertions below prove neither sentinel survives.
	jsonConfig, err := json.Marshal(c) //nolint:gosec // G117: Secret.MarshalJSON redacts every credential field; this test verifies it
	if err != nil {
		t.Fatalf("json.Marshal(Config): %v", err)
	}
	text, err := c.AgentToken.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}

	var logged strings.Builder
	slog.New(slog.NewTextHandler(&logged, nil)).Info("resolved configuration", "agentToken", c.AgentToken, "config", c)

	renderings := map[string]string{
		"%v on Config":     fmt.Sprintf("%v", c),
		"%+v on Config":    fmt.Sprintf("%+v", c),
		"%#v on Config":    fmt.Sprintf("%#v", c),
		"%v on Secret":     fmt.Sprintf("%v", c.AgentToken),
		"%s on Secret":     fmt.Sprintf("%s", c.AgentToken), //nolint:staticcheck // S1025: deliberately exercising the %s verb (fmt.Sprintf, not Secret.String) to prove this specific stringification path is also redacted
		"%q on Secret":     fmt.Sprintf("%q", c.AgentToken),
		"%#v on Secret":    fmt.Sprintf("%#v", c.AgentToken),
		"String()":         c.AgentToken.String(),
		"GoString()":       c.AgentToken.GoString(),
		"MarshalText":      string(text),
		"json on Secret":   string(jsonSecret),
		"json on Config":   string(jsonConfig),
		"slog TextHandler": logged.String(),
	}
	for path, out := range renderings {
		if strings.Contains(out, sentinel) {
			t.Errorf("%s leaked the agent token: %s", path, out)
		}
		if strings.Contains(out, apiSentinel) {
			t.Errorf("%s leaked the operator API token: %s", path, out)
		}
		if !strings.Contains(out, redacted) {
			t.Errorf("%s = %s, want it to contain %s", path, out, redacted)
		}
	}
}

func TestSecret_IsZero(t *testing.T) {
	if !Secret("").IsZero() {
		t.Error("Secret(\"\").IsZero() = false, want true")
	}
	if Secret("x").IsZero() {
		t.Error("Secret(\"x\").IsZero() = true, want false")
	}
}
