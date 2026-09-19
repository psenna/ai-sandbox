package agent

import (
	"context"
	"encoding/json"
	"reflect"
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

	t.Run("harness=opencode persists and spawns the opencode container image", func(t *testing.T) {
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

		// #180 wires the harness into the spawned container: an opencode
		// agent must run the operator's configured opencode image, NOT the
		// ordinary claude-code image.
		var found bool
		for _, c := range f.Containers() {
			if c.Name == got.ContainerName {
				found = true
				if c.Image != testAgentImageOpenCode {
					t.Errorf("agent container image = %q, want the opencode agent image %q", c.Image, testAgentImageOpenCode)
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
	m, f, st := newTestManager(t, 5)

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

	// The recreated container's image must still be resolved from the
	// RECORD's harness (opencode), not the request's (which never names a
	// harness at all) -- proving Update calls resolveAgentImageRef with
	// harnessOf(a), not req.Harness.
	var found bool
	for _, c := range f.Containers() {
		if c.Name == updated.ContainerName {
			found = true
			if c.Image != testAgentImageOpenCode {
				t.Errorf("recreated agent container image = %q, want the opencode agent image %q", c.Image, testAgentImageOpenCode)
			}
		}
	}
	if !found {
		t.Fatalf("no container named %q found among %v", updated.ContainerName, f.Containers())
	}
}

// TestFirstInvocationArgs is the exhaustive table for the one function that
// decides a session-resumption flag: every harness (including the "" default,
// which must behave exactly like "claude-code") x every AutoMode x every
// sessionStart. Two closing assertions pin the safety property #180 exists
// for: opencode never sees "--resume" (fatal on opencode -- issue #181's
// review), and claude-code never sees "--auto" (opencode's flag spelling).
func TestFirstInvocationArgs(t *testing.T) {
	cases := []struct {
		name    string
		harness string
		auto    string
		start   sessionStart
		want    []string
	}{
		{"default harness (\"\"), auto off, fresh", "", config.AutoModeOff, sessionFresh, nil},
		{"default harness (\"\"), auto off, continue", "", config.AutoModeOff, sessionContinue, []string{"--continue"}},
		{"default harness (\"\"), auto off, resume", "", config.AutoModeOff, sessionResume, []string{"--resume"}},
		{"default harness (\"\"), auto on, fresh", "", config.AutoModeOn, sessionFresh, []string{"--permission-mode", "auto"}},
		{"default harness (\"\"), auto on, continue", "", config.AutoModeOn, sessionContinue, []string{"--permission-mode", "auto", "--continue"}},
		{"default harness (\"\"), auto on, resume", "", config.AutoModeOn, sessionResume, []string{"--permission-mode", "auto", "--resume"}},

		{"claude-code, auto off, fresh", config.HarnessClaudeCode, config.AutoModeOff, sessionFresh, nil},
		{"claude-code, auto off, continue", config.HarnessClaudeCode, config.AutoModeOff, sessionContinue, []string{"--continue"}},
		{"claude-code, auto off, resume", config.HarnessClaudeCode, config.AutoModeOff, sessionResume, []string{"--resume"}},
		{"claude-code, auto on, fresh", config.HarnessClaudeCode, config.AutoModeOn, sessionFresh, []string{"--permission-mode", "auto"}},
		{"claude-code, auto on, continue", config.HarnessClaudeCode, config.AutoModeOn, sessionContinue, []string{"--permission-mode", "auto", "--continue"}},
		{"claude-code, auto on, resume", config.HarnessClaudeCode, config.AutoModeOn, sessionResume, []string{"--permission-mode", "auto", "--resume"}},

		{"opencode, auto off, fresh", config.HarnessOpenCode, config.AutoModeOff, sessionFresh, nil},
		{"opencode, auto off, continue", config.HarnessOpenCode, config.AutoModeOff, sessionContinue, []string{"--continue"}},
		{"opencode, auto off, resume", config.HarnessOpenCode, config.AutoModeOff, sessionResume, []string{"--continue"}},
		{"opencode, auto on, fresh", config.HarnessOpenCode, config.AutoModeOn, sessionFresh, []string{"--auto"}},
		{"opencode, auto on, continue", config.HarnessOpenCode, config.AutoModeOn, sessionContinue, []string{"--auto", "--continue"}},
		{"opencode, auto on, resume", config.HarnessOpenCode, config.AutoModeOn, sessionResume, []string{"--auto", "--continue"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := store.Agent{Harness: tc.harness, AutoMode: tc.auto}
			got := firstInvocationArgs(a, tc.start)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("firstInvocationArgs(harness=%q auto=%q start=%v) = %v, want %v", tc.harness, tc.auto, tc.start, got, tc.want)
			}
		})
	}

	t.Run("no opencode result ever contains --resume", func(t *testing.T) {
		for _, auto := range []string{config.AutoModeOff, config.AutoModeOn} {
			for _, start := range []sessionStart{sessionFresh, sessionContinue, sessionResume} {
				a := store.Agent{Harness: config.HarnessOpenCode, AutoMode: auto}
				got := firstInvocationArgs(a, start)
				for _, arg := range got {
					if arg == "--resume" {
						t.Errorf("firstInvocationArgs(opencode, auto=%q, start=%v) = %v, must never contain --resume (fatal on opencode)", auto, start, got)
					}
				}
			}
		}
	})

	t.Run("no claude-code result ever contains --auto", func(t *testing.T) {
		for _, harness := range []string{"", config.HarnessClaudeCode} {
			for _, auto := range []string{config.AutoModeOff, config.AutoModeOn} {
				for _, start := range []sessionStart{sessionFresh, sessionContinue, sessionResume} {
					a := store.Agent{Harness: harness, AutoMode: auto}
					got := firstInvocationArgs(a, start)
					for _, arg := range got {
						if arg == "--auto" {
							t.Errorf("firstInvocationArgs(harness=%q, auto=%q, start=%v) = %v, must never contain --auto (that is opencode's spelling)", harness, auto, start, got)
						}
					}
				}
			}
		}
	})
}

// TestAgentSpec_ImagePerHarness covers agentSpec's Image field for every
// harness, with and without a per-agent pinned override: the override always
// wins verbatim, and absent an override each harness gets its own default
// image (agentImageFor).
func TestAgentSpec_ImagePerHarness(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	rb := resolvedBackend{kind: config.BackendOllama, model: "m", fastModel: "f", ollamaURL: "http://ollama:11434"}

	cases := []struct {
		name          string
		harness       string
		imageOverride string
		want          string
	}{
		{"default harness (\"\"), no override", "", "", testAgentImageClaudeCode},
		{"default harness (\"\"), pinned override", "", "example.com/pinned:v1", "example.com/pinned:v1"},
		{"claude-code, no override", config.HarnessClaudeCode, "", testAgentImageClaudeCode},
		{"claude-code, pinned override", config.HarnessClaudeCode, "example.com/pinned:v2", "example.com/pinned:v2"},
		{"opencode, no override", config.HarnessOpenCode, "", testAgentImageOpenCode},
		{"opencode, pinned override", config.HarnessOpenCode, "example.com/pinned-oc:v1", "example.com/pinned-oc:v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := store.Agent{
				ID: "agt_x", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n",
				Harness: tc.harness, Image: tc.imageOverride,
			}
			spec, err := m.agentSpec(a, rb)
			if err != nil {
				t.Fatalf("agentSpec: %v", err)
			}
			if spec.Image != tc.want {
				t.Errorf("spec.Image = %q, want %q", spec.Image, tc.want)
			}
		})
	}
}

// TestResolveAgentImageRef_PerHarness covers resolveAgentImageRef's three
// outcomes (empty tag, a valid pinned tag, a malformed tag) against each
// harness's own default image repository.
func TestResolveAgentImageRef_PerHarness(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	for _, harness := range []string{config.HarnessClaudeCode, config.HarnessOpenCode} {
		t.Run(harness+": empty tag returns that harness's default image", func(t *testing.T) {
			want := m.agentImageFor(harness)
			got, err := m.resolveAgentImageRef("", harness)
			if err != nil || got != want {
				t.Errorf("resolveAgentImageRef(\"\", %q) = (%q, %v), want (%q, nil)", harness, got, err, want)
			}
		})
		t.Run(harness+": a valid pinned tag substitutes into that harness's repo", func(t *testing.T) {
			want := RepoWithoutTag(m.agentImageFor(harness)) + ":20260101-120000"
			got, err := m.resolveAgentImageRef("20260101-120000", harness)
			if err != nil || got != want {
				t.Errorf("resolveAgentImageRef(valid, %q) = (%q, %v), want (%q, nil)", harness, got, err, want)
			}
		})
		t.Run(harness+": a malformed tag is IsInvalidImageTag", func(t *testing.T) {
			if _, err := m.resolveAgentImageRef("bad tag!!", harness); !IsInvalidImageTag(err) {
				t.Errorf("resolveAgentImageRef(invalid, %q) err = %v, want IsInvalidImageTag", harness, err)
			}
		})
	}
}

// TestAgentSpec_CmdPerHarness proves agentSpec's Cmd is always
// [tmuxBootPath, <firstInvocationArgs output>], for every harness and every
// session-start.
func TestAgentSpec_CmdPerHarness(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	rb := resolvedBackend{kind: config.BackendOllama, model: "m", fastModel: "f", ollamaURL: "http://ollama:11434"}
	startName := map[sessionStart]string{sessionFresh: "fresh", sessionContinue: "continue", sessionResume: "resume"}

	for _, harness := range []string{config.HarnessClaudeCode, config.HarnessOpenCode} {
		for _, start := range []sessionStart{sessionFresh, sessionContinue, sessionResume} {
			t.Run(harness+"/"+startName[start], func(t *testing.T) {
				a := store.Agent{ID: "agt_x", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n", Harness: harness}
				args := firstInvocationArgs(a, start)
				spec, err := m.agentSpec(a, rb, args...)
				if err != nil {
					t.Fatalf("agentSpec: %v", err)
				}
				if len(spec.Cmd) == 0 || spec.Cmd[0] != tmuxBootPath {
					t.Fatalf("spec.Cmd = %v, want it to start with %q", spec.Cmd, tmuxBootPath)
				}
				// append([]string{tmuxBootPath}, args...) always yields a
				// non-nil (possibly empty) slice via spec.Cmd[1:], even when
				// args itself is nil (auto off, sessionFresh) -- compare by
				// length+contents rather than reflect.DeepEqual, which treats
				// nil and an empty slice as unequal.
				if len(spec.Cmd[1:]) != len(args) || !reflect.DeepEqual(spec.Cmd[1:], append([]string{}, args...)) {
					t.Errorf("spec.Cmd[1:] = %v, want firstInvocationArgs output %v", spec.Cmd[1:], args)
				}
			})
		}
	}
}

// TestAgentSpec_ConfigVolumeMountPerHarness proves the SAME per-agent config
// volume is mounted at different paths depending on harness: the ordinary
// Claude config path for claude-code, the opencode XDG data path for
// opencode. The workspace mount is identical for both.
func TestAgentSpec_ConfigVolumeMountPerHarness(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	rb := resolvedBackend{kind: config.BackendOllama, model: "m", fastModel: "f", ollamaURL: "http://ollama:11434"}

	cases := []struct {
		harness    string
		wantTarget string
	}{
		{config.HarnessClaudeCode, configMount},
		{config.HarnessOpenCode, opencodeDataMount},
	}
	for _, tc := range cases {
		t.Run(tc.harness, func(t *testing.T) {
			a := store.Agent{ID: "agt_x", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n", Harness: tc.harness}
			spec, err := m.agentSpec(a, rb)
			if err != nil {
				t.Fatalf("agentSpec: %v", err)
			}
			if len(spec.Mounts) < 2 {
				t.Fatalf("spec.Mounts = %+v, want at least 2 mounts", spec.Mounts)
			}
			workspace := spec.Mounts[0]
			if workspace.Source != "w" || workspace.Target != workspaceMount {
				t.Errorf("workspace mount = %+v, want source=w target=%q (identical for both harnesses)", workspace, workspaceMount)
			}
			cfgVol := spec.Mounts[1]
			if cfgVol.Source != "cc" || cfgVol.Target != tc.wantTarget {
				t.Errorf("config volume mount = %+v, want source=cc target=%q", cfgVol, tc.wantTarget)
			}
		})
	}
}

// TestAgentEnv_Opencode is the acceptance-criteria test for an opencode
// agent's environment: the generated OPENCODE_CONFIG_CONTENT and
// XDG_DATA_HOME are present and correct, and every claude-code-only variable
// is absent -- including the two tuning knobs and OAuth/API-key vars, which
// are deliberately set to non-empty values on the input record so their
// absence from the output is a meaningful assertion, not a vacuous one.
func TestAgentEnv_Opencode(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	a := store.Agent{
		ID: "agt_oc", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n",
		Harness:              config.HarnessOpenCode,
		Backend:              config.BackendOllama,
		AutoCompactThreshold: "85",
		MaxContextTokens:     "200000",
	}
	rb := resolvedBackend{kind: config.BackendOllama, model: "opus-m", fastModel: "fast-m", ollamaURL: "http://ollama:11434"}
	env, err := m.agentEnv(a, rb)
	if err != nil {
		t.Fatalf("agentEnv: %v", err)
	}

	cfgContent, ok := env["OPENCODE_CONFIG_CONTENT"]
	if !ok {
		t.Fatal("OPENCODE_CONFIG_CONTENT absent, want present for an opencode agent")
	}
	if !json.Valid([]byte(cfgContent)) {
		t.Fatalf("OPENCODE_CONFIG_CONTENT is not valid JSON: %s", cfgContent)
	}
	var decoded struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string `json:"baseURL"`
			} `json:"options"`
		} `json:"provider"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(cfgContent), &decoded); err != nil {
		t.Fatalf("json.Unmarshal(OPENCODE_CONFIG_CONTENT): %v", err)
	}
	ollama, ok := decoded.Provider["ollama"]
	if !ok {
		t.Fatalf("decoded provider[ollama] absent: %s", cfgContent)
	}
	if ollama.Options.BaseURL != "http://ollama:11434/v1" {
		t.Errorf("provider.ollama.options.baseURL = %q, want %q", ollama.Options.BaseURL, "http://ollama:11434/v1")
	}
	if decoded.Model != "ollama/opus-m" {
		t.Errorf("model = %q, want %q", decoded.Model, "ollama/opus-m")
	}

	if got := env["XDG_DATA_HOME"]; got != opencodeDataMount {
		t.Errorf("XDG_DATA_HOME = %q, want %q", got, opencodeDataMount)
	}

	for _, k := range []string{
		"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_ATTRIBUTION_HEADER",
		"CLAUDE_AUTO_COMPACT_THRESHOLD", "CLAUDE_CODE_MAX_CONTEXT_TOKENS",
		"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL", "CLAUDE_CODE_OAUTH_TOKEN",
	} {
		if _, ok := env[k]; ok {
			t.Errorf("env[%s] present for an opencode agent, want absent", k)
		}
	}

	for _, k := range []string{"AGENT_TOKEN", "GIT_PROXY_URL", "DOCKER_HOST", "TERM", "LANG"} {
		if _, ok := env[k]; !ok {
			t.Errorf("env[%s] absent, want present (harness-agnostic base var)", k)
		}
	}
}

// TestAgentEnv_ClaudeCodeUnchanged is the regression guard: a claude-code
// agent's environment (both the "" default and the explicit harness name)
// must be byte-identical to what it was before #180 -- every claude-code
// variable present and correct, and none of the new opencode-only variables
// leaking in.
func TestAgentEnv_ClaudeCodeUnchanged(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	rb := resolvedBackend{kind: config.BackendOllama, model: "opus-m", fastModel: "fast-m", ollamaURL: "http://ollama:11434"}

	for _, harness := range []string{"", config.HarnessClaudeCode} {
		t.Run("harness="+harness, func(t *testing.T) {
			a := store.Agent{
				ID: "agt_cc", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n",
				Harness: harness, AutoCompactThreshold: "85", MaxContextTokens: "200000",
			}
			env, err := m.agentEnv(a, rb)
			if err != nil {
				t.Fatalf("agentEnv: %v", err)
			}
			wantEq(t, env, "CLAUDE_CONFIG_DIR", configMount)
			wantEq(t, env, "CLAUDE_CODE_ATTRIBUTION_HEADER", "0")
			wantEq(t, env, "CLAUDE_AUTO_COMPACT_THRESHOLD", "85")
			wantEq(t, env, "CLAUDE_CODE_MAX_CONTEXT_TOKENS", "200000")
			wantEq(t, env, "ANTHROPIC_BASE_URL", "http://ollama:11434")
			wantEq(t, env, "ANTHROPIC_MODEL", "opus-m")
			wantEq(t, env, "ANTHROPIC_DEFAULT_OPUS_MODEL", "opus-m")
			wantEq(t, env, "ANTHROPIC_DEFAULT_SONNET_MODEL", "fast-m")
			wantEq(t, env, "ANTHROPIC_DEFAULT_HAIKU_MODEL", "fast-m")

			for _, k := range []string{"OPENCODE_CONFIG_CONTENT", "XDG_DATA_HOME"} {
				if _, ok := env[k]; ok {
					t.Errorf("env[%s] present for a claude-code agent, want absent", k)
				}
			}
		})
	}
}

// TestAgentEnv_OpencodeNeedsOllamaConfig proves an opencode agent whose
// resolved backend carries no ollama URL fails LOUDLY -- agentEnv returns a
// non-nil error (not a panic, not a silently-wrong env map) -- and that
// agentSpec propagates that error rather than returning a zero-value spec as
// if the container were fine to create.
func TestAgentEnv_OpencodeNeedsOllamaConfig(t *testing.T) {
	m, _, _ := newTestManager(t, 5)
	a := store.Agent{
		ID: "agt_oc_bad", ContainerName: "c", WorkspaceVolume: "w", ClaudeConfigVolume: "cc", DinernetName: "n",
		Harness: config.HarnessOpenCode,
	}
	rb := resolvedBackend{kind: config.BackendOllama, model: "opus-m", fastModel: "fast-m", ollamaURL: ""}

	if _, err := m.agentEnv(a, rb); err == nil {
		t.Fatal("agentEnv with an empty ollama URL = nil error, want a non-nil error")
	}

	spec, err := m.agentSpec(a, rb)
	if err == nil {
		t.Fatalf("agentSpec with an empty ollama URL = nil error (spec %+v), want it to propagate agentEnv's error", spec)
	}
}
