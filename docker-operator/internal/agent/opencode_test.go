package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestOpencodeConfigJSON_Shape pins the exact byte-for-byte output of
// opencodeConfigJSON for this repo's actual configured Ollama defaults (see
// config.defaultAgentModel / config.defaultAgentFastModel). Map keys in
// encoding/json come out lexicographically sorted, and '-' (0x2D) sorts
// before ':' (0x3A), so "glm-5.3-flash:cloud" precedes "glm-5.3:cloud" in
// the models map.
func TestOpencodeConfigJSON_Shape(t *testing.T) {
	got, err := opencodeConfigJSON("http://ollama:11434", "glm-5.3:cloud", "glm-5.3-flash:cloud")
	if err != nil {
		t.Fatalf("opencodeConfigJSON: %v", err)
	}

	want := `{"provider":{"ollama":{"npm":"@ai-sdk/openai-compatible","options":{"baseURL":"http://ollama:11434/v1"},"models":{"glm-5.3-flash:cloud":{},"glm-5.3:cloud":{}}}},"model":"ollama/glm-5.3:cloud","small_model":"ollama/glm-5.3-flash:cloud"}`

	if got != want {
		t.Errorf("opencodeConfigJSON =\n%s\nwant\n%s", got, want)
	}
}

// TestOpencodeConfigJSON_RoundTrips confirms the output is valid JSON and
// navigates the decoded map to check the fields a later issue's caller
// depends on.
func TestOpencodeConfigJSON_RoundTrips(t *testing.T) {
	got, err := opencodeConfigJSON("http://ollama:11434", "glm-5.3:cloud", "glm-5.3-flash:cloud")
	if err != nil {
		t.Fatalf("opencodeConfigJSON: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	provider, ok := decoded["provider"].(map[string]any)
	if !ok {
		t.Fatalf("decoded[provider] = %#v, want map[string]any", decoded["provider"])
	}
	ollama, ok := provider["ollama"].(map[string]any)
	if !ok {
		t.Fatalf("provider[ollama] = %#v, want map[string]any", provider["ollama"])
	}
	npm, ok := ollama["npm"].(string)
	if !ok || npm != "@ai-sdk/openai-compatible" {
		t.Fatalf("provider.ollama.npm = %#v, want %q", ollama["npm"], "@ai-sdk/openai-compatible")
	}
	options, ok := ollama["options"].(map[string]any)
	if !ok {
		t.Fatalf("provider.ollama.options = %#v, want map[string]any", ollama["options"])
	}
	baseURL, ok := options["baseURL"].(string)
	if !ok || baseURL != "http://ollama:11434/v1" {
		t.Fatalf("provider.ollama.options.baseURL = %#v, want %q", options["baseURL"], "http://ollama:11434/v1")
	}
	models, ok := ollama["models"].(map[string]any)
	if !ok {
		t.Fatalf("provider.ollama.models = %#v, want map[string]any", ollama["models"])
	}
	if len(models) != 2 {
		t.Errorf("len(provider.ollama.models) = %d, want 2", len(models))
	}
}

// TestOpencodeConfigJSON_Edges covers the boundary behaviors: trailing
// slash normalization, model/fast-model collapse, small_model omission,
// and the two error paths.
func TestOpencodeConfigJSON_Edges(t *testing.T) {
	t.Run("a trailing slash on the ollama URL does not double up before /v1", func(t *testing.T) {
		got, err := opencodeConfigJSON("http://ollama:11434/", "m1", "m2")
		if err != nil {
			t.Fatalf("opencodeConfigJSON: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(got), &decoded); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		provider := decoded["provider"].(map[string]any)
		ollama := provider["ollama"].(map[string]any)
		options := ollama["options"].(map[string]any)
		if baseURL, _ := options["baseURL"].(string); baseURL != "http://ollama:11434/v1" {
			t.Errorf("baseURL = %q, want %q", baseURL, "http://ollama:11434/v1")
		}
	})

	t.Run("model == fast model collapses to a single models entry", func(t *testing.T) {
		got, err := opencodeConfigJSON("http://ollama:11434", "m1", "m1")
		if err != nil {
			t.Fatalf("opencodeConfigJSON: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(got), &decoded); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		provider := decoded["provider"].(map[string]any)
		ollama := provider["ollama"].(map[string]any)
		models := ollama["models"].(map[string]any)
		if len(models) != 1 {
			t.Errorf("len(models) = %d, want 1", len(models))
		}
		if model, _ := decoded["model"].(string); model != "ollama/m1" {
			t.Errorf("model = %q, want %q", model, "ollama/m1")
		}
		if smallModel, _ := decoded["small_model"].(string); smallModel != "ollama/m1" {
			t.Errorf("small_model = %q, want %q", smallModel, "ollama/m1")
		}
	})

	t.Run("an empty fast model omits small_model entirely", func(t *testing.T) {
		got, err := opencodeConfigJSON("http://ollama:11434", "m1", "")
		if err != nil {
			t.Fatalf("opencodeConfigJSON: %v", err)
		}
		if strings.Contains(got, "small_model") {
			t.Errorf("output contains small_model, want it omitted: %s", got)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(got), &decoded); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		provider := decoded["provider"].(map[string]any)
		ollama := provider["ollama"].(map[string]any)
		models := ollama["models"].(map[string]any)
		if len(models) != 1 {
			t.Errorf("len(models) = %d, want 1", len(models))
		}
	})

	t.Run("an empty ollama URL is an error", func(t *testing.T) {
		if _, err := opencodeConfigJSON("", "m1", "m2"); err == nil {
			t.Fatalf("opencodeConfigJSON err = nil, want an error")
		}
	})

	t.Run("an empty model is an error", func(t *testing.T) {
		if _, err := opencodeConfigJSON("http://ollama:11434", "", "m2"); err == nil {
			t.Fatalf("opencodeConfigJSON err = nil, want an error")
		}
	})
}
