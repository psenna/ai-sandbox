package agent

import (
	"context"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// TestResolveBackend covers the request -> resolvedBackend mapping and the
// two error paths internal/api maps to 4xx.
func TestResolveBackend(t *testing.T) {
	ctx := context.Background()

	t.Run("default: no backend in the request uses the operator default", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		rb, err := m.resolveBackend(ctx, CreateRequest{})
		if err != nil {
			t.Fatalf("resolveBackend: %v", err)
		}
		if rb.kind != config.BackendOllama {
			t.Errorf("kind = %q, want %q (the config default)", rb.kind, config.BackendOllama)
		}
		if rb.model != "test-opus-model" || rb.fastModel != "test-fast-model" {
			t.Errorf("models = %q/%q, want the operator defaults", rb.model, rb.fastModel)
		}
	})

	t.Run("ollama with per-agent model overrides", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		rb, err := m.resolveBackend(ctx, CreateRequest{
			Backend: config.BackendOllama, Model: "custom-opus", FastModel: "custom-fast",
		})
		if err != nil {
			t.Fatalf("resolveBackend: %v", err)
		}
		if rb.model != "custom-opus" || rb.fastModel != "custom-fast" {
			t.Errorf("models = %q/%q, want the request overrides", rb.model, rb.fastModel)
		}
	})

	t.Run("ollama: one override, one default", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		rb, err := m.resolveBackend(ctx, CreateRequest{Backend: config.BackendOllama, Model: "only-opus"})
		if err != nil {
			t.Fatalf("resolveBackend: %v", err)
		}
		if rb.model != "only-opus" || rb.fastModel != "test-fast-model" {
			t.Errorf("models = %q/%q, want the override + the default", rb.model, rb.fastModel)
		}
	})

	t.Run("anthropic with a stored api key", func(t *testing.T) {
		m, _, st := newTestManager(t, 5)
		if err := st.SetAnthropicAuth(ctx, store.AnthropicKindAPIKey, "apikey-xyz"); err != nil {
			t.Fatalf("SetAnthropicAuth: %v", err)
		}
		rb, err := m.resolveBackend(ctx, CreateRequest{Backend: config.BackendAnthropic})
		if err != nil {
			t.Fatalf("resolveBackend: %v", err)
		}
		if rb.kind != config.BackendAnthropic || rb.apiKey != "apikey-xyz" || rb.oauthToken != "" {
			t.Errorf("rb = %+v, want kind=anthropic apiKey=apikey-xyz oauthToken=\"\"", rb)
		}
		if rb.model != "" || rb.fastModel != "" {
			t.Errorf("rb models = %q/%q, want both empty for anthropic", rb.model, rb.fastModel)
		}
	})

	t.Run("anthropic with a stored oauth token", func(t *testing.T) {
		m, _, st := newTestManager(t, 5)
		if err := st.SetAnthropicAuth(ctx, store.AnthropicKindOAuth, "oat-abc"); err != nil {
			t.Fatalf("SetAnthropicAuth: %v", err)
		}
		rb, err := m.resolveBackend(ctx, CreateRequest{Backend: config.BackendAnthropic})
		if err != nil {
			t.Fatalf("resolveBackend: %v", err)
		}
		if rb.apiKey != "" || rb.oauthToken != "oat-abc" {
			t.Errorf("rb = %+v, want apiKey=\"\" oauthToken=oat-abc", rb)
		}
	})

	t.Run("anthropic with no stored credential is ErrNoAnthropicAuth", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		_, err := m.resolveBackend(ctx, CreateRequest{Backend: config.BackendAnthropic})
		if !IsNoAnthropicAuth(err) {
			t.Fatalf("resolveBackend err = %v, want IsNoAnthropicAuth", err)
		}
	})

	t.Run("an unknown backend is ErrInvalidBackend", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		_, err := m.resolveBackend(ctx, CreateRequest{Backend: "vertex"})
		if !IsInvalidBackend(err) {
			t.Fatalf("resolveBackend err = %v, want IsInvalidBackend", err)
		}
	})
}

// TestAgentEnv_Backend checks the credential/model-routing half of the
// container environment for each backend.
func TestAgentEnv_Backend(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	base := store.Agent{ID: "agt_env", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n"}

	t.Run("ollama routes every tier and blanks the api key", func(t *testing.T) {
		env := m.agentEnv(base, resolvedBackend{kind: config.BackendOllama, model: "opus-m", fastModel: "fast-m"})
		wantEq(t, env, "ANTHROPIC_BASE_URL", "http://ollama:11434")
		wantEq(t, env, "ANTHROPIC_AUTH_TOKEN", "test-anthropic-auth")
		wantEq(t, env, "ANTHROPIC_MODEL", "opus-m")
		wantEq(t, env, "ANTHROPIC_DEFAULT_OPUS_MODEL", "opus-m")
		wantEq(t, env, "ANTHROPIC_DEFAULT_SONNET_MODEL", "fast-m")
		wantEq(t, env, "ANTHROPIC_DEFAULT_HAIKU_MODEL", "fast-m")
		wantEq(t, env, "ANTHROPIC_API_KEY", "")
		if _, ok := env["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
			t.Errorf("CLAUDE_CODE_OAUTH_TOKEN is set for an ollama agent, want it absent")
		}
	})

	t.Run("anthropic api-key: no base url, no model overrides", func(t *testing.T) {
		env := m.agentEnv(base, resolvedBackend{kind: config.BackendAnthropic, apiKey: "apikey-live"})
		wantEq(t, env, "ANTHROPIC_API_KEY", "apikey-live")
		wantEq(t, env, "CLAUDE_CODE_OAUTH_TOKEN", "")
		for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_AUTH_TOKEN"} {
			if _, ok := env[k]; ok {
				t.Errorf("%s is set for an anthropic agent, want it absent (real Anthropic defaults)", k)
			}
		}
	})

	t.Run("anthropic oauth: token set, api key blank", func(t *testing.T) {
		env := m.agentEnv(base, resolvedBackend{kind: config.BackendAnthropic, oauthToken: "oat-live"})
		wantEq(t, env, "CLAUDE_CODE_OAUTH_TOKEN", "oat-live")
		wantEq(t, env, "ANTHROPIC_API_KEY", "")
	})
}

func wantEq(t *testing.T, env map[string]string, key, want string) {
	t.Helper()
	if got, ok := env[key]; !ok || got != want {
		t.Errorf("env[%q] = %q (present=%v), want %q", key, got, ok, want)
	}
}

// TestCreate_PersistsBackend runs a full Create through the fake and checks
// the resolved backend/models land on the record; the error paths must not
// consume a slot or leave a Docker resource behind.
func TestCreate_PersistsBackend(t *testing.T) {
	ctx := context.Background()

	t.Run("ollama defaults", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		got, err := m.Create(ctx, CreateRequest{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.Backend != config.BackendOllama || got.Model != "test-opus-model" || got.FastModel != "test-fast-model" {
			t.Errorf("record = backend %q model %q/%q, want the ollama defaults", got.Backend, got.Model, got.FastModel)
		}
	})

	t.Run("anthropic with a stored credential", func(t *testing.T) {
		m, _, st := newTestManager(t, 5)
		if err := st.SetAnthropicAuth(ctx, store.AnthropicKindOAuth, "oat-1"); err != nil {
			t.Fatalf("SetAnthropicAuth: %v", err)
		}
		got, err := m.Create(ctx, CreateRequest{Backend: config.BackendAnthropic})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.Backend != config.BackendAnthropic || got.Model != "" || got.FastModel != "" {
			t.Errorf("record = backend %q model %q/%q, want anthropic with empty models", got.Backend, got.Model, got.FastModel)
		}
	})

	t.Run("anthropic with no credential: ErrNoAnthropicAuth, nothing created", func(t *testing.T) {
		m, f, st := newTestManager(t, 5)
		before := snapshotCounts(f)
		_, err := m.Create(ctx, CreateRequest{Backend: config.BackendAnthropic})
		if !IsNoAnthropicAuth(err) {
			t.Fatalf("Create err = %v, want IsNoAnthropicAuth", err)
		}
		if after := snapshotCounts(f); after != before {
			t.Errorf("docker resources = %+v, want unchanged baseline %+v", after, before)
		}
		if agents, _ := st.List(ctx); len(agents) != 0 {
			t.Errorf("store records = %v, want none (no slot consumed)", agents)
		}
	})

	t.Run("invalid backend: ErrInvalidBackend, nothing created", func(t *testing.T) {
		m, f, st := newTestManager(t, 5)
		before := snapshotCounts(f)
		_, err := m.Create(ctx, CreateRequest{Backend: "bogus"})
		if !IsInvalidBackend(err) {
			t.Fatalf("Create err = %v, want IsInvalidBackend", err)
		}
		if after := snapshotCounts(f); after != before {
			t.Errorf("docker resources = %+v, want unchanged baseline %+v", after, before)
		}
		if agents, _ := st.List(ctx); len(agents) != 0 {
			t.Errorf("store records = %v, want none", agents)
		}
	})
}
