package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

func decodeTemplateList(t *testing.T, body []byte) []store.Template {
	t.Helper()
	var resp templateListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decoding template list from %q: %v", body, err)
	}
	return resp.Templates
}

func TestCreateTemplate(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/templates", templateRequest{
		Name:    "my template",
		Backend: "ollama",
		Repo:    "owner/repo.git",
	})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
	}
	var tmpl store.Template
	if err := json.Unmarshal(rec.Body.Bytes(), &tmpl); err != nil {
		t.Fatalf("decoding template from %q: %v", rec.Body, err)
	}
	if tmpl.Name != "my template" || tmpl.Backend != "ollama" || tmpl.Repo != "owner/repo.git" || tmpl.ID == "" {
		t.Errorf("created template = %+v, want a non-empty ID and the requested fields", tmpl)
	}
	if got := rec.Header().Get("Location"); got != "/api/templates/"+tmpl.ID {
		t.Errorf("Location header = %q, want %q", got, "/api/templates/"+tmpl.ID)
	}
}

func TestCreateTemplate_MissingName(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/templates", templateRequest{Backend: "ollama"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
	}
	env := decodeEnvelope(t, rec)
	if env.Error.Code != CodeMissingField || env.Error.Field != "name" {
		t.Errorf("error = %+v, want code %q field %q", env.Error, CodeMissingField, "name")
	}
}

func TestCreateTemplate_InvalidBackend(t *testing.T) {
	// Proves validateAgentFields is genuinely reused: an invalid backend must
	// be rejected the same way it is for POST /api/agents, before the
	// manager is ever called.
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/templates", templateRequest{Name: "t", Backend: "not-a-backend"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeInvalidParam {
		t.Errorf("error code = %q, want %q", got, CodeInvalidParam)
	}
}

func TestCreateTemplate_AnthropicBackendRejectsOllamaFields(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/templates", templateRequest{Name: "t", Backend: "anthropic", Model: "opus"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
	}
}

func TestCreateTemplate_DuplicateName(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	first := doJSON(t, h, "POST", "/api/templates", templateRequest{Name: "shared"})
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want %d; body: %s", first.Code, http.StatusCreated, first.Body)
	}

	rec := doJSON(t, h, "POST", "/api/templates", templateRequest{Name: "shared"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body)
	}
	env := decodeEnvelope(t, rec)
	if env.Error.Code != CodeDuplicateName || env.Error.Field != "name" {
		t.Errorf("error = %+v, want code %q field %q", env.Error, CodeDuplicateName, "name")
	}
}

func TestListTemplates(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	doJSON(t, h, "POST", "/api/templates", templateRequest{Name: "one"})
	doJSON(t, h, "POST", "/api/templates", templateRequest{Name: "two"})

	rec := doJSON(t, h, "GET", "/api/templates", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	got := decodeTemplateList(t, rec.Body.Bytes())
	if len(got) != 2 {
		t.Fatalf("templates = %v, want 2 entries", got)
	}
}

func TestListTemplates_Empty(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/templates", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	got := decodeTemplateList(t, rec.Body.Bytes())
	if got == nil {
		t.Fatal("templates decoded as nil, want a non-nil empty slice (JSON [])")
	}
	if len(got) != 0 {
		t.Fatalf("templates = %v, want empty", got)
	}
}

func TestUpdateTemplate(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	created := doJSON(t, h, "POST", "/api/templates", templateRequest{Name: "original", Backend: "ollama"})
	var tmpl store.Template
	if err := json.Unmarshal(created.Body.Bytes(), &tmpl); err != nil {
		t.Fatalf("decoding created template: %v", err)
	}

	rec := doJSON(t, h, "PUT", "/api/templates/"+tmpl.ID, templateRequest{
		Name: "renamed", Description: "new description", Backend: "anthropic",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	var got store.Template
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding updated template: %v", err)
	}
	if got.ID != tmpl.ID || got.Name != "renamed" || got.Description != "new description" || got.Backend != "anthropic" {
		t.Errorf("updated template = %+v, unexpected field values", got)
	}
}

func TestUpdateTemplate_NotFound(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PUT", "/api/templates/tpl_missing", templateRequest{Name: "t"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeNotFound {
		t.Errorf("error code = %q, want %q", got, CodeNotFound)
	}
}

func TestUpdateTemplate_DuplicateName(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	doJSON(t, h, "POST", "/api/templates", templateRequest{Name: "taken"})
	createdOther := doJSON(t, h, "POST", "/api/templates", templateRequest{Name: "other"})
	var other store.Template
	if err := json.Unmarshal(createdOther.Body.Bytes(), &other); err != nil {
		t.Fatalf("decoding created template: %v", err)
	}

	rec := doJSON(t, h, "PUT", "/api/templates/"+other.ID, templateRequest{Name: "taken"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeDuplicateName {
		t.Errorf("error code = %q, want %q", got, CodeDuplicateName)
	}
}

func TestDeleteTemplate(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	created := doJSON(t, h, "POST", "/api/templates", templateRequest{Name: "t"})
	var tmpl store.Template
	if err := json.Unmarshal(created.Body.Bytes(), &tmpl); err != nil {
		t.Fatalf("decoding created template: %v", err)
	}

	rec := doJSON(t, h, "DELETE", "/api/templates/"+tmpl.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}

	list := doJSON(t, h, "GET", "/api/templates", nil)
	if got := decodeTemplateList(t, list.Body.Bytes()); len(got) != 0 {
		t.Fatalf("templates after delete = %v, want empty", got)
	}
}

func TestDeleteTemplate_IsIdempotent(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "DELETE", "/api/templates/tpl_never_existed", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
}

// TestDeleteTemplate_InUse is a regression test for a bug caught by Task 6's
// manual smoke check (not by any existing unit test, since fakeManager's
// DeleteTemplate never synthesizes store.ErrTemplateInUse on its own):
// handleDeleteTemplate used to route every mgr.DeleteTemplate error through
// internalError, turning the expected "still referenced by a CronAgent"
// conflict into a 500 instead of the 409 handleCreateTemplate/
// handleUpdateTemplate already give the same sentinel via templateError.
func TestDeleteTemplate_InUse(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	mgr.deleteTemplateErr = fmt.Errorf("deleting template %q: referenced by cron agent %q: %w", "tpl_1", "ca_1", store.ErrTemplateInUse)

	rec := doJSON(t, h, "DELETE", "/api/templates/tpl_1", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeTemplateInUse {
		t.Errorf("error code = %q, want %q", got, CodeTemplateInUse)
	}
}

// TestCreateTemplate_RejectsUnknownScheduleField proves templateRequest no
// longer accepts the removed scheduling fields: decodeJSON's reject-unknown-
// fields behavior (pre-existing, unrelated to this task) turns a "schedule"
// key in the body into a 400, since templateRequest declares no such field
// anymore.
func TestCreateTemplate_RejectsUnknownScheduleField(t *testing.T) {
	h := newTestHandler(newFakeManager(5), nil)
	req := httptest.NewRequest("POST", "/api/templates", strings.NewReader(`{"name":"t","schedule":"0 9 * * *"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (schedule is no longer a template field)", rec.Code, http.StatusBadRequest)
	}
}

func TestTemplatesMethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	cases := []struct{ method, path string }{
		{"DELETE", "/api/templates"},
		{"PUT", "/api/templates"},
		{"POST", "/api/templates/tpl_00000001"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := doJSON(t, h, tc.method, tc.path, nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusMethodNotAllowed, rec.Body)
			}
			if got := decodeEnvelope(t, rec).Error.Code; got != CodeMethodNotAllowed {
				t.Errorf("error code = %q, want %q", got, CodeMethodNotAllowed)
			}
		})
	}
}
