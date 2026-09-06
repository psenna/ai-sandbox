package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

func decode(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decoding %q: %v", string(b), err)
	}
}

// --- POST /api/agents: backend + model fields -----------------------------

func TestCreate_BackendFields(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents", map[string]any{
		"backend": "ollama", "model": "custom-opus", "fast_model": "custom-fast",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body)
	}
	a := decodeAgent(t, rec)
	if a.Backend != "ollama" || a.Model != "custom-opus" || a.FastModel != "custom-fast" {
		t.Errorf("created agent = backend %q model %q/%q, want the request values", a.Backend, a.Model, a.FastModel)
	}
}

func TestCreate_UnknownBackendIs400(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents", map[string]any{"backend": "vertex"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
	}
	env := decodeEnvelope(t, rec)
	if env.Error.Code != CodeInvalidParam || env.Error.Field != "backend" {
		t.Errorf("error = %+v, want invalid_param on field \"backend\"", env.Error)
	}
}

func TestCreate_AnthropicWithModelOverrideIs400(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents", map[string]any{"backend": "anthropic", "model": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeInvalidParam {
		t.Errorf("error code = %q, want %q", got, CodeInvalidParam)
	}
}

func TestCreate_NoAnthropicAuthIs409(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.createErr = agent.ErrNoAnthropicAuth
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents", map[string]any{"backend": "anthropic"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rec.Code, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeNoAnthropicAuth {
		t.Errorf("error code = %q, want %q", got, CodeNoAnthropicAuth)
	}
}

func TestCreate_InvalidBackendFromManagerIs400(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.createErr = agent.ErrInvalidBackend
	h := newTestHandler(mgr, dockerclienttest.New())

	// A backend the handler's own check let through (it only rejects
	// non-empty unknowns) but the manager rejected.
	rec := doJSON(t, h, "POST", "/api/agents", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
	}
}

// --- /api/anthropic/auth -------------------------------------------------

func TestAnthropicAuth_GetBeforeSet(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/anthropic/auth", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var resp anthropicAuthResponse
	decode(t, rec.Body.Bytes(), &resp)
	if resp.Configured || resp.Kind != "" || resp.UpdatedAt != nil {
		t.Errorf("resp = %+v, want configured=false, no kind, no updated_at", resp)
	}
}

func TestAnthropicAuth_PutThenGet(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PUT", "/api/anthropic/auth", map[string]any{"kind": "oauth", "value": "sk-ant-oat01-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var put anthropicAuthResponse
	decode(t, rec.Body.Bytes(), &put)
	if !put.Configured || put.Kind != "oauth" || put.UpdatedAt == nil {
		t.Errorf("PUT resp = %+v, want configured=true kind=oauth with updated_at", put)
	}

	rec = doJSON(t, h, "GET", "/api/anthropic/auth", nil)
	var got anthropicAuthResponse
	decode(t, rec.Body.Bytes(), &got)
	if !got.Configured || got.Kind != "oauth" {
		t.Errorf("GET resp = %+v, want configured=true kind=oauth", got)
	}

	// The secret must never appear in any response body.
	if strings.Contains(rec.Body.String(), "sk-ant-oat01-secret") {
		t.Errorf("GET body leaked the credential value: %s", rec.Body.String())
	}
}

func TestAnthropicAuth_PutValidation(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		code string
	}{
		{"unknown kind", map[string]any{"kind": "bearer", "value": "x"}, CodeInvalidParam},
		{"empty value", map[string]any{"kind": "oauth", "value": "  "}, CodeMissingField},
		{"whitespace-only value", map[string]any{"kind": "oauth", "value": "\n\t "}, CodeMissingField},
		{"api key without sk-ant- prefix", map[string]any{"kind": "api_key", "value": "nope"}, CodeInvalidParam},
		{"oauth token without sk-ant-oat01- prefix", map[string]any{"kind": "oauth", "value": "oat-nope"}, CodeInvalidParam},
		{"oauth token with an interior newline (wrapped paste)", map[string]any{"kind": "oauth", "value": "sk-ant-oat01-aaa\nbbb"}, CodeInvalidParam},
		{"api key with an interior space", map[string]any{"kind": "api_key", "value": "sk-ant-aaa bbb"}, CodeInvalidParam},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newFakeManager(5)
			h := newTestHandler(mgr, dockerclienttest.New())
			rec := doJSON(t, h, "PUT", "/api/anthropic/auth", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
			}
			if got := decodeEnvelope(t, rec).Error.Code; got != tc.code {
				t.Errorf("error code = %q, want %q", got, tc.code)
			}
			if mgr.anthropicSet {
				t.Errorf("a rejected PUT still stored a credential")
			}
		})
	}
}

func TestAnthropicAuth_PutAcceptsAValidAPIKey(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PUT", "/api/anthropic/auth", map[string]any{"kind": "api_key", "value": "sk-ant-abc123"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if !mgr.anthropicSet || mgr.anthropicKind != store.AnthropicKindAPIKey {
		t.Errorf("manager state = set=%v kind=%q, want set with api_key", mgr.anthropicSet, mgr.anthropicKind)
	}
}

// A credential pasted from a terminal typically carries a trailing newline;
// the handler must store the trimmed value so what lands in an agent's
// environment is a usable bearer, not "sk-ant-oat01-…\n".
func TestAnthropicAuth_PutTrimsSurroundingWhitespace(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PUT", "/api/anthropic/auth", map[string]any{"kind": "oauth", "value": "  sk-ant-oat01-secret\n"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if mgr.anthropicValue != "sk-ant-oat01-secret" {
		t.Errorf("stored value = %q, want it trimmed to %q", mgr.anthropicValue, "sk-ant-oat01-secret")
	}
}

func TestAnthropicAuth_Delete(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.anthropicSet = true
	mgr.anthropicKind = "oauth"
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "DELETE", "/api/anthropic/auth", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if mgr.anthropicSet {
		t.Errorf("credential still set after DELETE")
	}
	// Idempotent.
	rec = doJSON(t, h, "DELETE", "/api/anthropic/auth", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("second DELETE status = %d, want 200", rec.Code)
	}
}

func TestAnthropicAuth_MethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/anthropic/auth", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body: %s", rec.Code, rec.Body)
	}
}

func TestAnthropicAuth_GetErrorIs500(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.anthropicGetErr = errors.New("boltdb exploded")
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/anthropic/auth", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "boltdb exploded") {
		t.Errorf("500 body leaked the internal error: %s", rec.Body.String())
	}
}

// --- /api/anthropic/login ----------------------------------------------

func TestAnthropicLogin_StartStopStatus(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	// Not active initially.
	rec := doJSON(t, h, "GET", "/api/anthropic/login", nil)
	var got anthropicLoginResponse
	decode(t, rec.Body.Bytes(), &got)
	if got.Active || got.WS != "" {
		t.Fatalf("initial GET = %+v, want inactive with no ws", got)
	}

	// Start.
	rec = doJSON(t, h, "POST", "/api/anthropic/login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	decode(t, rec.Body.Bytes(), &got)
	if !got.Active || got.WS != "/ws/anthropic/login/terminal" {
		t.Fatalf("POST resp = %+v, want active with the ws path", got)
	}
	if !mgr.loginActive {
		t.Error("manager loginActive is false after POST")
	}

	// GET now reports active.
	rec = doJSON(t, h, "GET", "/api/anthropic/login", nil)
	decode(t, rec.Body.Bytes(), &got)
	if !got.Active {
		t.Error("GET after POST reports inactive")
	}

	// Delete.
	rec = doJSON(t, h, "DELETE", "/api/anthropic/login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", rec.Code)
	}
	if mgr.loginActive {
		t.Error("manager loginActive is true after DELETE")
	}
}

func TestAnthropicLogin_StartErrorIs500(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.loginStartErr = errors.New("no such network docker-operator-proxynet")
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/anthropic/login", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "docker-operator-proxynet") {
		t.Errorf("500 body leaked the internal error: %s", rec.Body.String())
	}
}

func TestAnthropicLogin_MethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PUT", "/api/anthropic/login", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestAnthropicAuth_PutTearsDownLogin: storing a credential means the
// setup-token helper has done its job, so PUT /api/anthropic/auth also
// removes the login container.
func TestAnthropicAuth_PutTearsDownLogin(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.loginActive = true
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PUT", "/api/anthropic/auth", map[string]any{"kind": "oauth", "value": "sk-ant-oat01-x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if mgr.loginActive {
		t.Error("login container still active after a successful PUT /api/anthropic/auth")
	}
}

// A login-teardown failure during PUT must not fail the request -- the
// credential was still stored.
func TestAnthropicAuth_PutSucceedsEvenIfLoginTeardownFails(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.loginActive = true
	mgr.loginStopErr = errors.New("daemon hiccup")
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PUT", "/api/anthropic/auth", map[string]any{"kind": "oauth", "value": "sk-ant-oat01-x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 despite the teardown failure; body: %s", rec.Code, rec.Body)
	}
	if !mgr.anthropicSet {
		t.Error("credential was not stored")
	}
}
