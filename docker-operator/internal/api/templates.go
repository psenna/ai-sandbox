package api

import (
	"net/http"
	"strings"

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
	}
}

// validateTemplateFields runs the template-specific check (a non-empty
// name) plus every createAgentRequest field check, reused via
// toAgentFieldsRequest.
func validateTemplateFields(req templateRequest) *apiErr {
	if strings.TrimSpace(req.Name) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"name" must not be empty`, "name"}
	}
	return validateAgentFields(req.toAgentFieldsRequest())
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
