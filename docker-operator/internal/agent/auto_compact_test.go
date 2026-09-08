package agent

import (
	"context"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// TestAgentEnv_AutoCompactThreshold pins the conditional-injection rule: the
// variable reaches the container only when a value was actually resolved. An
// empty per-agent value (with no operator default) omits it ENTIRELY -- it is
// not set to "" -- so the agent keeps Claude Code's own built-in default.
func TestAgentEnv_AutoCompactThreshold(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	rb := resolvedBackend{kind: config.BackendOllama, model: "o", fastModel: "f"}

	t.Run("a resolved value is injected", func(t *testing.T) {
		a := store.Agent{ID: "a", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n", AutoCompactThreshold: "85"}
		env := m.agentEnv(a, rb)
		wantEq(t, env, "CLAUDE_AUTO_COMPACT_THRESHOLD", "85")
	})

	t.Run("an empty value omits the variable entirely", func(t *testing.T) {
		a := store.Agent{ID: "a", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n"}
		env := m.agentEnv(a, rb)
		if _, ok := env["CLAUDE_AUTO_COMPACT_THRESHOLD"]; ok {
			t.Errorf("env[CLAUDE_AUTO_COMPACT_THRESHOLD] present, want absent when the value is empty")
		}
	})
}

func TestCreate_ResolvesAutoCompactThreshold(t *testing.T) {
	ctx := context.Background()

	t.Run("no request value and no operator default: omitted", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		got, err := m.Create(ctx, CreateRequest{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.AutoCompactThreshold != "" {
			t.Errorf("record.AutoCompactThreshold = %q, want empty", got.AutoCompactThreshold)
		}
	})

	t.Run("no request value falls back to the operator default", func(t *testing.T) {
		cfg := testConfig(5)
		cfg.AutoCompactThreshold = "70"
		m, _, _ := newTestManagerCfg(t, cfg)
		got, err := m.Create(ctx, CreateRequest{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.AutoCompactThreshold != "70" {
			t.Errorf("record.AutoCompactThreshold = %q, want the operator default", got.AutoCompactThreshold)
		}
	})

	t.Run("a per-agent value overrides the operator default", func(t *testing.T) {
		cfg := testConfig(5)
		cfg.AutoCompactThreshold = "70"
		m, _, _ := newTestManagerCfg(t, cfg)
		got, err := m.Create(ctx, CreateRequest{AutoCompactThreshold: "90"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.AutoCompactThreshold != "90" {
			t.Errorf("record.AutoCompactThreshold = %q, want the per-agent value", got.AutoCompactThreshold)
		}
	})
}
