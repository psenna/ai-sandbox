package agent

import (
	"context"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// TestResolveSpec_Harness covers the request -> resolvedSpec.harness mapping
// and the two error paths internal/api maps to 4xx, including the
// resolved-pair case an early request-only check cannot catch.
func TestResolveSpec_Harness(t *testing.T) {
	ctx := context.Background()

	t.Run("omitted harness defaults to claude-code", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		rs, err := m.resolveSpec(ctx, CreateRequest{})
		if err != nil {
			t.Fatalf("resolveSpec: %v", err)
		}
		if rs.harness != config.HarnessClaudeCode {
			t.Errorf("harness = %q, want %q (the built-in default)", rs.harness, config.HarnessClaudeCode)
		}
	})

	t.Run("opencode with ollama is ok", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		rs, err := m.resolveSpec(ctx, CreateRequest{Harness: config.HarnessOpenCode, Backend: config.BackendOllama})
		if err != nil {
			t.Fatalf("resolveSpec: %v", err)
		}
		if rs.harness != config.HarnessOpenCode {
			t.Errorf("harness = %q, want %q", rs.harness, config.HarnessOpenCode)
		}
	})

	t.Run("opencode with anthropic is IsIncompatibleHarness", func(t *testing.T) {
		m, _, st := newTestManager(t, 5)
		// Seed a credential so the only possible failure reason is the
		// harness/backend rule, not a missing Anthropic credential.
		if err := st.SetAnthropicAuth(ctx, store.AnthropicKindAPIKey, "apikey-xyz"); err != nil {
			t.Fatalf("SetAnthropicAuth: %v", err)
		}
		_, err := m.resolveSpec(ctx, CreateRequest{Harness: config.HarnessOpenCode, Backend: config.BackendAnthropic})
		if !IsIncompatibleHarness(err) {
			t.Fatalf("resolveSpec err = %v, want IsIncompatibleHarness", err)
		}
	})

	t.Run("opencode with no backend named on an anthropic-default operator is IsIncompatibleHarness", func(t *testing.T) {
		cfg := testConfig(5)
		cfg.DefaultBackend = config.BackendAnthropic
		m, _, st := newTestManagerCfg(t, cfg)
		if err := st.SetAnthropicAuth(ctx, store.AnthropicKindAPIKey, "apikey-xyz"); err != nil {
			t.Fatalf("SetAnthropicAuth: %v", err)
		}
		// This is exactly the case internal/api's early request-only check
		// cannot catch: the request names no backend at all, and only the
		// RESOLVED pair (opencode + the operator's anthropic default) is
		// incompatible.
		_, err := m.resolveSpec(ctx, CreateRequest{Harness: config.HarnessOpenCode})
		if !IsIncompatibleHarness(err) {
			t.Fatalf("resolveSpec err = %v, want IsIncompatibleHarness", err)
		}
	})

	t.Run("an unknown harness is IsInvalidHarness", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		_, err := m.resolveSpec(ctx, CreateRequest{Harness: "vertex-code"})
		if !IsInvalidHarness(err) {
			t.Fatalf("resolveSpec err = %v, want IsInvalidHarness", err)
		}
	})
}

// TestCreate_PersistsHarness runs a full Create through the fake and checks
// the resolved harness lands on the record; per issue #179's scoping, an
// opencode agent must still spawn the ordinary claude-code container exactly
// as today (actual harness-conditional spawn behaviour is issue #180), and
// the incompatible-pair error must not consume a slot or leave a Docker
// resource behind.
func TestCreate_PersistsHarness(t *testing.T) {
	ctx := context.Background()

	t.Run("no harness in the request persists as claude-code", func(t *testing.T) {
		m, _, st := newTestManager(t, 5)
		got, err := m.Create(ctx, CreateRequest{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.Harness != config.HarnessClaudeCode {
			t.Errorf("record Harness = %q, want %q", got.Harness, config.HarnessClaudeCode)
		}
		// Re-fetch from the store to confirm persistence, not just the
		// in-memory return value.
		stored, err := st.Get(ctx, got.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.Harness != config.HarnessClaudeCode {
			t.Errorf("stored Harness = %q, want %q", stored.Harness, config.HarnessClaudeCode)
		}
	})

	t.Run("harness=opencode persists and spawns the ordinary container", func(t *testing.T) {
		m, f, st := newTestManager(t, 5)
		got, err := m.Create(ctx, CreateRequest{Harness: config.HarnessOpenCode, Backend: config.BackendOllama})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.Harness != config.HarnessOpenCode {
			t.Errorf("record Harness = %q, want %q", got.Harness, config.HarnessOpenCode)
		}
		stored, err := st.Get(ctx, got.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.Harness != config.HarnessOpenCode {
			t.Errorf("stored Harness = %q, want %q", stored.Harness, config.HarnessOpenCode)
		}

		// Nothing opencode-specific happened to the spawned container: it
		// still runs the ordinary configured agent image (#180 has not
		// landed).
		var found bool
		for _, c := range f.Containers() {
			if c.Name == got.ContainerName {
				found = true
				if c.Image != testAgentImage {
					t.Errorf("agent container image = %q, want the ordinary agent image %q", c.Image, testAgentImage)
				}
			}
		}
		if !found {
			t.Fatalf("no container named %q found among %v", got.ContainerName, f.Containers())
		}
	})

	t.Run("opencode with anthropic: IsIncompatibleHarness, nothing created", func(t *testing.T) {
		m, f, st := newTestManager(t, 5)
		if err := st.SetAnthropicAuth(ctx, store.AnthropicKindAPIKey, "apikey-xyz"); err != nil {
			t.Fatalf("SetAnthropicAuth: %v", err)
		}
		before := snapshotCounts(f)
		_, err := m.Create(ctx, CreateRequest{Harness: config.HarnessOpenCode, Backend: config.BackendAnthropic})
		if !IsIncompatibleHarness(err) {
			t.Fatalf("Create err = %v, want IsIncompatibleHarness", err)
		}
		if after := snapshotCounts(f); after != before {
			t.Errorf("docker resources = %+v, want unchanged baseline %+v", after, before)
		}
		if agents, _ := st.List(ctx); len(agents) != 0 {
			t.Errorf("store records = %v, want none (no slot consumed)", agents)
		}
	})
}

// TestUpdate_PreservesHarness confirms Update's deliberate omission: an
// in-place update never changes an agent's harness, even when it changes
// some unrelated field -- this is what stops the (harness-unaware) web UI
// from silently downgrading an opencode agent back to claude-code on every
// update.
func TestUpdate_PreservesHarness(t *testing.T) {
	ctx := context.Background()
	m, _, st := newTestManager(t, 5)

	created, err := m.Create(ctx, CreateRequest{Name: "before", Harness: config.HarnessOpenCode, Backend: config.BackendOllama})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Harness != config.HarnessOpenCode {
		t.Fatalf("seeded agent Harness = %q, want %q", created.Harness, config.HarnessOpenCode)
	}

	updated, err := m.Update(ctx, created.ID, UpdateRequest{
		CreateRequest: CreateRequest{Name: "after", Backend: config.BackendOllama},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "after" {
		t.Fatalf("updated agent Name = %q, want %q", updated.Name, "after")
	}
	if updated.Harness != config.HarnessOpenCode {
		t.Errorf("updated agent Harness = %q, want unchanged %q", updated.Harness, config.HarnessOpenCode)
	}

	stored, err := st.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Harness != config.HarnessOpenCode {
		t.Errorf("stored Harness = %q, want unchanged %q", stored.Harness, config.HarnessOpenCode)
	}
}
