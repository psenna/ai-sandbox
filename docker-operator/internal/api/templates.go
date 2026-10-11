package api

import (
	"net/http"
	"strings"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// templateListResponse is the GET /api/templates body. Templates is never
// nil (JSON "[]" for no saved templates), matching fileListResponse's
// wrapped-array convention.
type templateListResponse struct {
	Templates []store.Template `json:"templates"`
}

// templateRequest is the POST /api/templates and PUT /api/templates/{id}
// body: a template's own Name/Description plus the same 9 create-form
// fields createAgentRequest carries.
//
// This deliberately does NOT embed createAgentRequest the way
// updateAgentRequest does: a template needs its own top-level Name/
// Description (the template's identity, shown in the UI's picker dropdown)
// while explicitly NOT wanting createAgentRequest's Name/Description at all
// -- embedding would silently shadow one pair. Declaring the 9 shared fields
// directly costs a little duplication but avoids that landmine.
type templateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`

	Backend              string `json:"backend"`
	Model                string `json:"model"`
	FastModel            string `json:"fast_model"`
	OllamaURL            string `json:"ollama_url"`
	Repo                 string `json:"repo"`
	AutoCompactThreshold string `json:"auto_compact_threshold"`
	MaxContextTokens     string `json:"max_context_tokens"`
	ImageTag             string `json:"image_tag"`
	AutoMode             string `json:"auto_mode"`

	Harness    string `json:"harness"`
	Schedule   string `json:"schedule"`
	Enabled    bool   `json:"enabled"`
	Task       string `json:"task"`
	OnComplete string `json:"on_complete"`
}

// toAgentFieldsRequest maps templateRequest's 9 shared fields onto
// createAgentRequest, so validateAgentFields can be reused verbatim instead
// of duplicating its checks.
func (t templateRequest) toAgentFieldsRequest() createAgentRequest {
	return createAgentRequest{
		Backend:              t.Backend,
		Model:                t.Model,
		FastModel:            t.FastModel,
		OllamaURL:            t.OllamaURL,
		Repo:                 t.Repo,
		AutoCompactThreshold: t.AutoCompactThreshold,
		MaxContextTokens:     t.MaxContextTokens,
		ImageTag:             t.ImageTag,
		AutoMode:             t.AutoMode,
	}
}

// toTemplateCreateSpec maps a validated templateRequest onto the store's
// spec type. ID is left empty; callers that need one (handleCreateTemplate)
// fill it in themselves -- CreateTemplate on the manager generates it.
func (t templateRequest) toTemplateCreateSpec() store.TemplateCreateSpec {
	return store.TemplateCreateSpec{
		Name:                 t.Name,
		Description:          t.Description,
		Backend:              t.Backend,
		Model:                t.Model,
		FastModel:            t.FastModel,
		OllamaURL:            t.OllamaURL,
		Repo:                 t.Repo,
		AutoCompactThreshold: t.AutoCompactThreshold,
		MaxContextTokens:     t.MaxContextTokens,
		ImageTag:             t.ImageTag,
		AutoMode:             t.AutoMode,

		Harness:    t.Harness,
		Schedule:   t.Schedule,
		Enabled:    t.Enabled,
		Task:       t.Task,
		OnComplete: t.OnComplete,
	}
}

// validOnComplete reports whether s is one of the three values ApplyOnComplete
// understands. Template-only: agents have no OnComplete concept, so this
// lives here rather than in internal/config alongside the agent-level
// validators.
func validOnComplete(s string) bool {
	return s == "stop" || s == "delete" || s == "leave"
}

// validateTemplateFields runs the template-specific check (a non-empty
// name) plus every createAgentRequest field check, reused via
// toAgentFieldsRequest. When Schedule is set, it also requires Harness, Task
// and OnComplete together, rejects malformed cron strings (parsed with the
// exact parser the ticker itself uses), and rejects the opencode harness (v1
// scheduling is Claude Code only). A template with no Schedule validates
// exactly as it did before this feature -- no new requirements for it.
func validateTemplateFields(req templateRequest) *apiErr {
	if strings.TrimSpace(req.Name) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"name" must not be empty`, "name"}
	}
	if ae := validateAgentFields(req.toAgentFieldsRequest()); ae != nil {
		return ae
	}
	if req.Schedule == "" {
		return nil
	}
	if req.Harness == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"harness" is required when "schedule" is set`, "harness"}
	}
	if !config.ValidHarness(req.Harness) {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"harness" must be "claude-code" or "opencode"`, "harness"}
	}
	if req.Harness == config.HarnessOpenCode {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `scheduling is not supported for the "opencode" harness yet`, "harness"}
	}
	if strings.TrimSpace(req.Task) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"task" is required when "schedule" is set`, "task"}
	}
	if !validOnComplete(req.OnComplete) {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"on_complete" must be "stop", "delete" or "leave"`, "on_complete"}
	}
	if _, err := agent.ParseSchedule(req.Schedule); err != nil {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"schedule" must be a valid standard 5-field cron expression`, "schedule"}
	}
	return nil
}

// templateError maps a store.Template sentinel error to the right HTTP
// response, mirroring notFoundOrInternal's shape for agents (a template has
// no agent.Manager-level error family to switch on, since it never touches
// Docker).
func (h *Handler) templateError(w http.ResponseWriter, action string, err error) {
	switch {
	case store.IsTemplateNotFound(err):
		writeError(w, http.StatusNotFound, CodeNotFound, "no such template", "")
	case store.IsTemplateNameExists(err):
		writeError(w, http.StatusConflict, CodeDuplicateName, "a template with that name already exists", "name")
	default:
		h.internalError(w, action, err)
	}
}

func (h *Handler) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := h.mgr.ListTemplates(r.Context())
	if err != nil {
		h.internalError(w, "listing templates", err)
		return
	}
	writeJSON(w, http.StatusOK, templateListResponse{Templates: templates})
}

func (h *Handler) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var req templateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	if ae := validateTemplateFields(req); ae != nil {
		ae.write(w)
		return
	}

	t, err := h.mgr.CreateTemplate(r.Context(), req.toTemplateCreateSpec())
	if err != nil {
		h.templateError(w, "creating template", err)
		return
	}
	w.Header().Set("Location", "/api/templates/"+t.ID)
	writeJSON(w, http.StatusCreated, t)
}

// handleUpdateTemplate replaces the whole template record: the create form
// submits every field every time, so this is PUT semantics, not a partial
// PATCH.
func (h *Handler) handleUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req templateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	if ae := validateTemplateFields(req); ae != nil {
		ae.write(w)
		return
	}

	t, err := h.mgr.UpdateTemplate(r.Context(), id, req.toTemplateCreateSpec())
	if err != nil {
		h.templateError(w, "updating template "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleDeleteTemplate always answers 200, whether or not the template
// existed, matching handleDelete's idempotent stance for an agent.
func (h *Handler) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.mgr.DeleteTemplate(r.Context(), id); err != nil {
		h.internalError(w, "deleting template "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "deleted"})
}

// scheduleRunListResponse is the GET /api/templates/{id}/runs body, same
// wrapped-array convention as templateListResponse.
type scheduleRunListResponse struct {
	Runs []store.ScheduleRun `json:"runs"`
}

func (h *Handler) handleListScheduleRuns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	runs, err := h.mgr.ListScheduleRunsByTemplate(r.Context(), id)
	if err != nil {
		h.internalError(w, "listing schedule runs for template "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, scheduleRunListResponse{Runs: runs})
}
