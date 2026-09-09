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

	t.Run("a resolved max-context value is injected as CLAUDE_CODE_MAX_CONTEXT_TOKENS", func(t *testing.T) {
		a := store.Agent{ID: "a", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n", MaxContextTokens: "200000"}
		env := m.agentEnv(a, rb)
		wantEq(t, env, "CLAUDE_CODE_MAX_CONTEXT_TOKENS", "200000")
	})

	t.Run("an empty value omits the variable entirely", func(t *testing.T) {
		a := store.Agent{ID: "a", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n"}
		env := m.agentEnv(a, rb)
		for _, name := range []string{"CLAUDE_AUTO_COMPACT_THRESHOLD", "CLAUDE_CODE_MAX_CONTEXT_TOKENS"} {
			if _, ok := env[name]; ok {
				t.Errorf("env[%s] present, want absent when the value is empty", name)
			}
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

// TestCreate_RejectsOutOfRangeAutoCompactThreshold pins the 50..100 rule at
// the create layer: an out-of-range resolved value (per-agent or operator
// default) is a caller mistake and must fail without consuming a MAX_AGENTS
// slot.
func TestCreate_RejectsOutOfRangeAutoCompactThreshold(t *testing.T) {
	ctx := context.Background()

	for _, bad := range []string{"49", "101", "abc", "85.5", "-20"} {
		t.Run("per-agent value "+bad, func(t *testing.T) {
			m, _, _ := newTestManager(t, 5)
			_, err := m.Create(ctx, CreateRequest{AutoCompactThreshold: bad})
			if !IsInvalidAutoCompactThreshold(err) {
				t.Fatalf("Create with AutoCompactThreshold=%q: err = %v, want an ErrInvalidAutoCompactThreshold", bad, err)
			}
		})
		t.Run("operator default "+bad, func(t *testing.T) {
			cfg := testConfig(5)
			cfg.AutoCompactThreshold = bad
			m, _, _ := newTestManagerCfg(t, cfg)
			_, err := m.Create(ctx, CreateRequest{})
			if !IsInvalidAutoCompactThreshold(err) {
				t.Fatalf("Create with operator default %q: err = %v, want an ErrInvalidAutoCompactThreshold", bad, err)
			}
		})
	}

	t.Run("the boundary values 50 and 100 are accepted", func(t *testing.T) {
		for _, good := range []string{"50", "100"} {
			m, _, _ := newTestManager(t, 5)
			got, err := m.Create(ctx, CreateRequest{AutoCompactThreshold: good})
			if err != nil {
				t.Fatalf("Create with AutoCompactThreshold=%q: %v", good, err)
			}
			if got.AutoCompactThreshold != good {
				t.Errorf("record.AutoCompactThreshold = %q, want %q", got.AutoCompactThreshold, good)
			}
		}
	})
}

// TestCreate_ResolvesMaxContextTokens pins the same resolve-and-fall-through
// rule for the max-context token budget: per-agent value, else the operator
// default, else omitted entirely.
func TestCreate_ResolvesMaxContextTokens(t *testing.T) {
	ctx := context.Background()

	t.Run("no request value and no operator default: omitted", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		got, err := m.Create(ctx, CreateRequest{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.MaxContextTokens != "" {
			t.Errorf("record.MaxContextTokens = %q, want empty", got.MaxContextTokens)
		}
	})

	t.Run("no request value falls back to the operator default", func(t *testing.T) {
		cfg := testConfig(5)
		cfg.MaxContextTokens = "150000"
		m, _, _ := newTestManagerCfg(t, cfg)
		got, err := m.Create(ctx, CreateRequest{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.MaxContextTokens != "150000" {
			t.Errorf("record.MaxContextTokens = %q, want the operator default", got.MaxContextTokens)
		}
	})

	t.Run("a per-agent value overrides the operator default", func(t *testing.T) {
		cfg := testConfig(5)
		cfg.MaxContextTokens = "150000"
		m, _, _ := newTestManagerCfg(t, cfg)
		got, err := m.Create(ctx, CreateRequest{MaxContextTokens: "200000"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.MaxContextTokens != "200000" {
			t.Errorf("record.MaxContextTokens = %q, want the per-agent value", got.MaxContextTokens)
		}
	})
}
