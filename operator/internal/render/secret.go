package render

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	acorev1 "k8s.io/client-go/applyconfigurations/core/v1"
)

// renderSecret renders the environment Secret holding the agent's process
// environment, projected from the class's referenced services and the
// credentials resolved by the caller. Rendered with data (not stringData) --
// stringData never persists as its own API field (the API server folds it
// into data on write), so a field manager tracking stringData would never
// actually own data and could not correct drift on it via server-side
// apply.
//
// Key set (see operator/README.md for the authoritative table, cross-checked
// against claude-code/entrypoint.sh and docker-compose.yaml's claude
// service): GIT_PROXY_HEADER and DEPENDAPROXY_TOKEN are deliberately NOT
// rendered -- the former is reconstructable by the agent image's own
// entrypoint from AGENT_TOKEN, and the latter is unused because DependaProxy
// auth is disabled in this stack.
func renderSecret(in Inputs) *acorev1.SecretApplyConfiguration {
	names := ChildNames(in.Env.Name)
	data := map[string][]byte{}

	if token := in.Credentials.GitProxyToken; token != "" {
		data["AGENT_TOKEN"] = []byte(token)
		data["GIT_PROXY_TOKEN"] = []byte(token)
	}

	if gp := in.Class.Spec.Services.GitProxy; gp != nil {
		data["GIT_PROXY_URL"] = []byte(gp.GitURL)
		data["GIT_PROXY_BROKER_URL"] = []byte(gp.BrokerURL)
	}

	data["GITHUB_REPO"] = []byte(in.Env.Spec.Repo)

	if dp := in.Class.Spec.Services.DependaProxy; dp != nil {
		if dp.NpmURL != "" {
			data["DEPENDAPROXY_URL"] = []byte(dp.NpmURL)
			data["NPM_CONFIG_REGISTRY"] = []byte(dp.NpmURL)
		}
		if dp.PypiURL != "" {
			data["DEPENDAPROXY_PYPI_URL"] = []byte(dp.PypiURL)
			data["PIP_INDEX_URL"] = []byte(strings.TrimSuffix(dp.PypiURL, "/") + "/simple")
		}
		if dp.GoproxyURL != "" {
			data["DEPENDAPROXY_GOPROXY_URL"] = []byte(dp.GoproxyURL)
			data["GOPROXY"] = []byte(dp.GoproxyURL)
		}
	}

	if ol := in.Class.Spec.Services.Ollama; ol != nil {
		data["ANTHROPIC_BASE_URL"] = []byte(ol.BaseURL)
	}
	data["ANTHROPIC_AUTH_TOKEN"] = []byte("ollama")
	data["ANTHROPIC_API_KEY"] = []byte("")

	if m := in.Class.Spec.Agent.Model; m != nil {
		if m.Default != "" {
			data["ANTHROPIC_MODEL"] = []byte(m.Default)
		}
		if m.Haiku != "" {
			data["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = []byte(m.Haiku)
		}
		if m.Sonnet != "" {
			data["ANTHROPIC_DEFAULT_SONNET_MODEL"] = []byte(m.Sonnet)
		}
		if m.Opus != "" {
			data["ANTHROPIC_DEFAULT_OPUS_MODEL"] = []byte(m.Opus)
		}
	}

	data["CLAUDE_CONFIG_DIR"] = []byte(AgentHomePath)
	data["CLAUDE_CODE_ATTRIBUTION_HEADER"] = []byte("0")

	// SANDBOX_SIDECAR_URL: the agent-facing base URL of the sandboxctl
	// control API (#27), loopback-only, reachable only from inside this
	// pod. Not a secret, but rendered here (alongside the rest of the
	// agent's process environment) rather than the ConfigMap, matching
	// where every other envFrom-consumed value already lives.
	data["SANDBOX_SIDECAR_URL"] = []byte(SidecarBaseURL)

	return acorev1.Secret(names.Secret, in.Env.Namespace).
		WithLabels(Labels(in.Env)).
		WithOwnerReferences(ownerReference(in.Env)).
		WithType(corev1.SecretTypeOpaque).
		WithData(data)
}
