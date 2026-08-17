package render

import (
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func renderSecretFor(t *testing.T, in Inputs) map[string][]byte {
	t.Helper()
	objs, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return objs.Secret.Data
}

func TestSecret_AlwaysPresentKeys(t *testing.T) {
	data := renderSecretFor(t, Inputs{Env: baseEnv("e"), Class: minimalClass()})
	for _, k := range []string{"GITHUB_REPO", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_ATTRIBUTION_HEADER"} {
		if _, ok := data[k]; !ok {
			t.Errorf("missing always-present key %q", k)
		}
	}
	if string(data["ANTHROPIC_AUTH_TOKEN"]) != "ollama" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q, want ollama", data["ANTHROPIC_AUTH_TOKEN"])
	}
	if string(data["ANTHROPIC_API_KEY"]) != "" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want empty", data["ANTHROPIC_API_KEY"])
	}
	if string(data["CLAUDE_CONFIG_DIR"]) != AgentHomePath {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", data["CLAUDE_CONFIG_DIR"], AgentHomePath)
	}
	if string(data["CLAUDE_CODE_ATTRIBUTION_HEADER"]) != "0" {
		t.Errorf("CLAUDE_CODE_ATTRIBUTION_HEADER = %q, want 0", data["CLAUDE_CODE_ATTRIBUTION_HEADER"])
	}
}

func TestSecret_NeverPresentKeys(t *testing.T) {
	data := renderSecretFor(t, Inputs{
		Env:         baseEnv("e"),
		Class:       fullClass(),
		Credentials: Credentials{GitProxyToken: "fake-token"},
	})
	for _, k := range []string{"GIT_PROXY_HEADER", "DEPENDAPROXY_TOKEN"} {
		if _, ok := data[k]; ok {
			t.Errorf("unexpected key %q rendered (should never be rendered)", k)
		}
	}
}

func TestSecret_GitProxyKeys(t *testing.T) {
	t.Run("absent when no gitProxy service", func(t *testing.T) {
		data := renderSecretFor(t, Inputs{Env: baseEnv("e"), Class: minimalClass(), Credentials: Credentials{GitProxyToken: "tok"}})
		for _, k := range []string{"GIT_PROXY_URL", "GIT_PROXY_BROKER_URL"} {
			if _, ok := data[k]; ok {
				t.Errorf("key %q present with no gitProxy service configured", k)
			}
		}
		// Token keys are keyed off Credentials, not the service block, so
		// they ARE present here.
		if string(data["AGENT_TOKEN"]) != "tok" || string(data["GIT_PROXY_TOKEN"]) != "tok" {
			t.Errorf("AGENT_TOKEN/GIT_PROXY_TOKEN not projected from Credentials")
		}
	})

	t.Run("absent when no credentials even with gitProxy service", func(t *testing.T) {
		data := renderSecretFor(t, Inputs{Env: baseEnv("e"), Class: fullClass()})
		for _, k := range []string{"AGENT_TOKEN", "GIT_PROXY_TOKEN"} {
			if _, ok := data[k]; ok {
				t.Errorf("key %q present with empty credentials", k)
			}
		}
		if string(data["GIT_PROXY_URL"]) != "http://git-proxy:8080" {
			t.Errorf("GIT_PROXY_URL = %q", data["GIT_PROXY_URL"])
		}
		if string(data["GIT_PROXY_BROKER_URL"]) != "http://git-proxy:8090" {
			t.Errorf("GIT_PROXY_BROKER_URL = %q", data["GIT_PROXY_BROKER_URL"])
		}
	})
}

func TestSecret_DependaProxyKeysIndependentlyGuarded(t *testing.T) {
	class := minimalClass()
	class.Spec.Services.DependaProxy = &v1alpha1.DependaProxyService{
		NpmURL: "http://dependaproxy:8080/npm",
		// PypiURL and GoproxyURL left empty.
	}
	data := renderSecretFor(t, Inputs{Env: baseEnv("e"), Class: class})

	if string(data["DEPENDAPROXY_URL"]) != "http://dependaproxy:8080/npm" {
		t.Errorf("DEPENDAPROXY_URL = %q", data["DEPENDAPROXY_URL"])
	}
	if string(data["NPM_CONFIG_REGISTRY"]) != "http://dependaproxy:8080/npm" {
		t.Errorf("NPM_CONFIG_REGISTRY = %q", data["NPM_CONFIG_REGISTRY"])
	}
	for _, k := range []string{"DEPENDAPROXY_PYPI_URL", "PIP_INDEX_URL", "DEPENDAPROXY_GOPROXY_URL", "GOPROXY"} {
		if _, ok := data[k]; ok {
			t.Errorf("key %q present despite empty source field", k)
		}
	}
}

func TestSecret_PipIndexURLTrailingSlash(t *testing.T) {
	class := minimalClass()
	class.Spec.Services.DependaProxy = &v1alpha1.DependaProxyService{
		PypiURL: "http://dependaproxy:8080/pypi/",
	}
	data := renderSecretFor(t, Inputs{Env: baseEnv("e"), Class: class})

	got := string(data["PIP_INDEX_URL"])
	want := "http://dependaproxy:8080/pypi/simple"
	if got != want {
		t.Errorf("PIP_INDEX_URL = %q, want %q", got, want)
	}
	if strings.Contains(got, "//simple") {
		t.Errorf("PIP_INDEX_URL has a double slash: %q", got)
	}
}

func TestSecret_ModelTiersIndependentlyGuarded(t *testing.T) {
	class := minimalClass()
	class.Spec.Agent.Model = &v1alpha1.ModelSpec{Default: "m-default"}
	data := renderSecretFor(t, Inputs{Env: baseEnv("e"), Class: class})

	if string(data["ANTHROPIC_MODEL"]) != "m-default" {
		t.Errorf("ANTHROPIC_MODEL = %q", data["ANTHROPIC_MODEL"])
	}
	for _, k := range []string{"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL"} {
		if _, ok := data[k]; ok {
			t.Errorf("key %q present despite unset tier", k)
		}
	}
}

func TestSecret_OllamaKeys(t *testing.T) {
	t.Run("absent when no ollama service", func(t *testing.T) {
		data := renderSecretFor(t, Inputs{Env: baseEnv("e"), Class: minimalClass()})
		if _, ok := data["ANTHROPIC_BASE_URL"]; ok {
			t.Error("ANTHROPIC_BASE_URL present with no ollama service configured")
		}
	})
	t.Run("present when ollama service set", func(t *testing.T) {
		data := renderSecretFor(t, Inputs{Env: baseEnv("e"), Class: fullClass()})
		if string(data["ANTHROPIC_BASE_URL"]) != "http://ollama:11434" {
			t.Errorf("ANTHROPIC_BASE_URL = %q", data["ANTHROPIC_BASE_URL"])
		}
	})
}

func TestSecret_TokenValuesMatchVerbatimAndAreUnique(t *testing.T) {
	const token = "fake-bearer-value-xyz" //nolint:gosec // G101: deliberately fake test fixture value, not a real credential
	data := renderSecretFor(t, Inputs{
		Env:         baseEnv("e"),
		Class:       fullClass(),
		Credentials: Credentials{GitProxyToken: token},
	})

	if string(data["AGENT_TOKEN"]) != token {
		t.Errorf("AGENT_TOKEN = %q, want %q", data["AGENT_TOKEN"], token)
	}
	if string(data["GIT_PROXY_TOKEN"]) != token {
		t.Errorf("GIT_PROXY_TOKEN = %q, want %q", data["GIT_PROXY_TOKEN"], token)
	}
	for k, v := range data {
		if k == "AGENT_TOKEN" || k == "GIT_PROXY_TOKEN" {
			continue
		}
		if string(v) == token {
			t.Errorf("key %q unexpectedly equals the credential token verbatim", k)
		}
	}
}
