package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// opencodeConfig, opencodeProvider, opencodeProviderOptions, opencodeModel
// mirror the shape opencode's own config.json schema expects for a custom
// OpenAI-compatible provider. Field declaration order matters: encoding/json
// emits struct fields in declaration order (map keys come out sorted), which
// is what keeps opencodeConfigJSON's output byte-stable for the exact-match
// test.
type opencodeConfig struct {
	Provider   map[string]opencodeProvider `json:"provider"`
	Model      string                      `json:"model"`
	SmallModel string                      `json:"small_model,omitempty"`
}

type opencodeProvider struct {
	NPM     string                   `json:"npm"`
	Options opencodeProviderOptions  `json:"options"`
	Models  map[string]opencodeModel `json:"models"`
}

type opencodeProviderOptions struct {
	BaseURL string `json:"baseURL"`
}

// opencodeModel is deliberately empty: opencode accepts {} for a model entry
// and infers the rest from the provider's npm package.
type opencodeModel struct{}

const (
	opencodeProviderID  = "ollama" // the provider key AND the "<provider>/<model>" prefix
	opencodeProviderNPM = "@ai-sdk/openai-compatible"
)

// opencodeConfigJSON builds the opencode.json content for an agent using the
// Ollama backend, pointed at Ollama's native OpenAI-compatible /v1 endpoint
// (the opposite of how Claude Code talks to Ollama, which impersonates an
// Anthropic endpoint via ANTHROPIC_BASE_URL -- opencode has no such shim, so
// this must speak opencode's real provider config shape). The caller (a
// later issue) sets the returned string as the OPENCODE_CONFIG_CONTENT env
// var -- there is no bind mount and no OPENCODE_CONFIG_DIR file.
func opencodeConfigJSON(ollamaURL, model, fastModel string) (string, error) {
	if ollamaURL == "" {
		return "", fmt.Errorf("generating opencode.json: the ollama URL must not be empty")
	}
	if model == "" {
		return "", fmt.Errorf("generating opencode.json: the model must not be empty")
	}

	// config.ValidOllamaURL accepts a trailing slash, so trim it first --
	// otherwise a URL like "http://ollama:11434/" would double up the slash
	// before "v1".
	baseURL := strings.TrimRight(ollamaURL, "/") + "/v1"

	models := map[string]opencodeModel{model: {}}
	if fastModel != "" {
		models[fastModel] = opencodeModel{}
	}

	cfg := opencodeConfig{
		Provider: map[string]opencodeProvider{
			opencodeProviderID: {
				NPM:     opencodeProviderNPM,
				Options: opencodeProviderOptions{BaseURL: baseURL},
				Models:  models,
			},
		},
		Model: opencodeProviderID + "/" + model,
	}
	// Only set SmallModel when fastModel is non-empty: combined with
	// omitempty, a fast-model-less agent emits no small_model key rather
	// than a dangling "ollama/", letting opencode apply its own default.
	if fastModel != "" {
		cfg.SmallModel = opencodeProviderID + "/" + fastModel
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshaling opencode.json: %w", err)
	}
	return string(b), nil
}
