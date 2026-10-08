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

// --- /api/anthropic/accounts ----------------------------------------------

func TestAnthropicAccounts_ListEmpty(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/anthropic/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/anthropic/accounts status = %d; want 200", rec.Code)
	}
	var body anthropicAccountsListResponse
	decode(t, rec.Body.Bytes(), &body)
	if len(body.Accounts) != 0 {
		t.Fatalf("Accounts = %v; want empty", body.Accounts)
	}
}

func TestAnthropicAccounts_CreateThenList(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/anthropic/accounts", map[string]any{"name": "Work", "kind": "api_key", "value": "sk-ant-abc123"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/anthropic/accounts status = %d, body = %s; want 201", rec.Code, rec.Body)
	}
	var created anthropicAccountView
	decode(t, rec.Body.Bytes(), &created)
	if created.Name != "Work" || created.Kind != "api_key" || !created.IsDefault {
		t.Fatalf("created account = %+v; want Name=Work Kind=api_key IsDefault=true (first account)", created)
	}
	// Never leak the value.
	if strings.Contains(rec.Body.String(), "sk-ant-abc123") {
		t.Fatalf("response body leaked the account value: %s", rec.Body.String())
	}
	if len(mgr.anthropicAccounts) != 1 || mgr.anthropicAccounts[0].Value != "sk-ant-abc123" {
		t.Fatalf("fakeManager.anthropicAccounts = %+v; want one account with the real value stored server-side", mgr.anthropicAccounts)
	}
}

func TestAnthropicAccountsCreate_Validation(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		code string
	}{
		{"missing name", map[string]any{"kind": "api_key", "value": "sk-ant-abc123"}, CodeMissingField},
		{"unknown kind", map[string]any{"name": "Work", "kind": "bearer", "value": "x"}, CodeInvalidParam},
		{"empty value", map[string]any{"name": "Work", "kind": "oauth", "value": "  "}, CodeMissingField},
		{"whitespace-only value", map[string]any{"name": "Work", "kind": "oauth", "value": "\n\t "}, CodeMissingField},
		{"api key without sk-ant- prefix", map[string]any{"name": "Work", "kind": "api_key", "value": "nope"}, CodeInvalidParam},
		{"oauth token without sk-ant-oat01- prefix", map[string]any{"name": "Work", "kind": "oauth", "value": "oat-nope"}, CodeInvalidParam},
		{"oauth token with an interior newline (wrapped paste)", map[string]any{"name": "Work", "kind": "oauth", "value": "sk-ant-oat01-aaa\nbbb"}, CodeInvalidParam},
		{"api key with an interior space", map[string]any{"name": "Work", "kind": "api_key", "value": "sk-ant-aaa bbb"}, CodeInvalidParam},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newFakeManager(5)
			h := newTestHandler(mgr, dockerclienttest.New())
			rec := doJSON(t, h, "POST", "/api/anthropic/accounts", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
			}
			if got := decodeEnvelope(t, rec).Error.Code; got != tc.code {
				t.Errorf("error code = %q, want %q", got, tc.code)
			}
			if len(mgr.anthropicAccounts) != 0 {
				t.Errorf("a rejected create still stored an account: %+v", mgr.anthropicAccounts)
			}
		})
	}
}

// A credential pasted from a terminal typically carries a trailing newline;
// the handler must store the trimmed value so what lands in an agent's
// environment is a usable bearer, not "sk-ant-oat01-…\n".
func TestAnthropicAccountsCreate_TrimsSurroundingWhitespace(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/anthropic/accounts", map[string]any{"name": "Work", "kind": "oauth", "value": "  sk-ant-oat01-secret\n"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body)
	}
	if len(mgr.anthropicAccounts) != 1 || mgr.anthropicAccounts[0].Value != "sk-ant-oat01-secret" {
		t.Fatalf("stored accounts = %+v, want one account with the value trimmed to %q", mgr.anthropicAccounts, "sk-ant-oat01-secret")
	}
}

func TestAnthropicAccountDelete_IsIdempotent(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.anthropicAccounts = []store.AnthropicAccount{{ID: "anc_1", Name: "Work", Kind: store.AnthropicKindAPIKey}}
	mgr.anthropicDefaultID = "anc_1"
	h := newTestHandler(mgr, dockerclienttest.New())

	// An existing id: deleted, 200.
	rec := doJSON(t, h, "DELETE", "/api/anthropic/accounts/anc_1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if len(mgr.anthropicAccounts) != 0 {
		t.Errorf("account still present after DELETE: %+v", mgr.anthropicAccounts)
	}
	// The same id again -- idempotent, still 200.
	rec = doJSON(t, h, "DELETE", "/api/anthropic/accounts/anc_1", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("second DELETE status = %d, want 200", rec.Code)
	}
	// An id that never existed -- also 200.
	rec = doJSON(t, h, "DELETE", "/api/anthropic/accounts/anc_never_existed", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("DELETE of an unknown id status = %d, want 200", rec.Code)
	}
}

func TestAnthropicAccountSetDefault_UnknownIs404(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PUT", "/api/anthropic/accounts/anc_missing/default", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body)
	}
}

func TestAnthropicAccountSetDefault_Succeeds(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.anthropicAccounts = []store.AnthropicAccount{
		{ID: "anc_1", Name: "Work", Kind: store.AnthropicKindAPIKey},
		{ID: "anc_2", Name: "Personal", Kind: store.AnthropicKindOAuth},
	}
	mgr.anthropicDefaultID = "anc_1"
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PUT", "/api/anthropic/accounts/anc_2/default", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body)
	}

	rec = doJSON(t, h, "GET", "/api/anthropic/accounts", nil)
	var body anthropicAccountsListResponse
	decode(t, rec.Body.Bytes(), &body)
	byID := map[string]anthropicAccountView{}
	for _, a := range body.Accounts {
		byID[a.ID] = a
	}
	if !byID["anc_2"].IsDefault {
		t.Errorf("anc_2 = %+v, want IsDefault=true after being set as default", byID["anc_2"])
	}
	if byID["anc_1"].IsDefault {
		t.Errorf("anc_1 = %+v, want IsDefault=false after anc_2 became the default", byID["anc_1"])
	}
}

func TestAnthropicAccounts_MethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PATCH", "/api/anthropic/accounts", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body: %s", rec.Code, rec.Body)
	}
}

func TestAnthropicAccountsList_ErrorIs500(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.anthropicListErr = errors.New("boltdb exploded")
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/anthropic/accounts", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "boltdb exploded") {
		t.Errorf("500 body leaked the internal error: %s", rec.Body.String())
	}
}

func TestAnthropicAccountsCreate_ErrorIs500(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.anthropicCreateErr = errors.New("boltdb exploded")
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/anthropic/accounts", map[string]any{"name": "Work", "kind": "api_key", "value": "sk-ant-abc123"})
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

// TestAnthropicAccountsCreate_TearsDownLogin: creating an account means a
// running `claude setup-token` helper (if this was its finish step) has
// done its job, so POST /api/anthropic/accounts also removes the login
// container.
func TestAnthropicAccountsCreate_TearsDownLogin(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.loginActive = true
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/anthropic/accounts", map[string]any{"name": "Work", "kind": "oauth", "value": "sk-ant-oat01-x"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201; body: %s", rec.Code, rec.Body)
	}
	if mgr.loginActive {
		t.Error("login container still active after a successful POST /api/anthropic/accounts")
	}
}

// A login-teardown failure during create must not fail the request -- the
// account was still stored.
func TestAnthropicAccountsCreate_SucceedsEvenIfLoginTeardownFails(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.loginActive = true
	mgr.loginStopErr = errors.New("daemon hiccup")
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/anthropic/accounts", map[string]any{"name": "Work", "kind": "oauth", "value": "sk-ant-oat01-x"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201 despite the teardown failure; body: %s", rec.Code, rec.Body)
	}
	if len(mgr.anthropicAccounts) != 1 {
		t.Error("account was not stored")
	}
}
