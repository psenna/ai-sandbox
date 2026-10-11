package api

import (
	"net/http"
	"strings"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// cronAgentListResponse is the GET /api/cron-agents body. CronAgents is
// never nil, matching templateListResponse's wrapped-array convention.
type cronAgentListResponse struct {
	CronAgents []store.CronAgent `json:"cron_agents"`
}

// cronAgentRequest is the POST /api/cron-agents and PUT
// /api/cron-agents/{id} body.
type cronAgentRequest struct {
	Name       string `json:"name"`
	TemplateID string `json:"template_id"`
	Schedule   string `json:"schedule"`
	Enabled    bool   `json:"enabled"`
	Task       string `json:"task"`
	OnComplete string `json:"on_complete"`
}

func (r cronAgentRequest) toCronAgentCreateSpec() store.CronAgentCreateSpec {
	return store.CronAgentCreateSpec{
		Name: r.Name, TemplateID: r.TemplateID, Schedule: r.Schedule,
		Enabled: r.Enabled, Task: r.Task, OnComplete: r.OnComplete,
	}
}

// validOnComplete reports whether s is one of the three values
// ApplyOnComplete understands. Moved here from templates.go, now that
// OnComplete lives on CronAgent, not Template.
func validOnComplete(s string) bool {
	return s == "stop" || s == "delete" || s == "leave"
}

// validateCronAgentFields runs the request's own shape checks: Name and
// TemplateID required, Schedule required and parseable with the same
// parser the ticker uses, Task required, OnComplete one of the three known
// values. It does NOT check that TemplateID references an existing
// Template, or that Template's Harness -- those need a store lookup, so
// they live in the handler (handleCreateCronAgent/handleUpdateCronAgent),
// the same division of labor validateAgentFields/CreateTemplate already
// have between pure field-shape checks and store-backed existence checks.
func validateCronAgentFields(req cronAgentRequest) *apiErr {
	if strings.TrimSpace(req.Name) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"name" must not be empty`, "name"}
	}
	if strings.TrimSpace(req.TemplateID) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"template_id" must not be empty`, "template_id"}
	}
	if strings.TrimSpace(req.Schedule) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"schedule" must not be empty`, "schedule"}
	}
	if _, err := agent.ParseSchedule(req.Schedule); err != nil {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"schedule" must be a valid standard 5-field cron expression`, "schedule"}
	}
	if strings.TrimSpace(req.Task) == "" {
		return &apiErr{http.StatusBadRequest, CodeMissingField, `"task" must not be empty`, "task"}
	}
	if !validOnComplete(req.OnComplete) {
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"on_complete" must be "stop", "delete" or "leave"`, "on_complete"}
	}
	return nil
}

// cronAgentError maps a store.CronAgent sentinel error to the right HTTP
// response, mirroring templateError's shape.
func (h *Handler) cronAgentError(w http.ResponseWriter, action string, err error) {
	switch {
	case store.IsCronAgentNotFound(err):
		writeError(w, http.StatusNotFound, CodeNotFound, "no such cron agent", "")
	default:
		h.internalError(w, action, err)
	}
}

// lookUpTemplateForCronAgent resolves req.TemplateID and checks its
// Harness, writing the response itself on failure. Returns ok=false when
// it already wrote a response -- the caller returns immediately in that
// case. Shared by handleCreateCronAgent and handleUpdateCronAgent.
func (h *Handler) lookUpTemplateForCronAgent(w http.ResponseWriter, r *http.Request, templateID string) (ok bool) {
	tmpl, err := h.mgr.GetTemplate(r.Context(), templateID)
	if err != nil {
		if store.IsTemplateNotFound(err) {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"template_id" does not reference an existing template`, "template_id")
			return false
		}
		h.internalError(w, "looking up template for cron agent", err)
		return false
	}
	if tmpl.Harness == "opencode" {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, `scheduling is not supported for the "opencode" harness yet`, "template_id")
		return false
	}
	return true
}

func (h *Handler) handleListCronAgents(w http.ResponseWriter, r *http.Request) {
	cronAgents, err := h.mgr.ListCronAgents(r.Context())
	if err != nil {
		h.internalError(w, "listing cron agents", err)
		return
	}
	writeJSON(w, http.StatusOK, cronAgentListResponse{CronAgents: cronAgents})
}

func (h *Handler) handleCreateCronAgent(w http.ResponseWriter, r *http.Request) {
	var req cronAgentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	if ae := validateCronAgentFields(req); ae != nil {
		ae.write(w)
		return
	}
	if !h.lookUpTemplateForCronAgent(w, r, req.TemplateID) {
		return
	}

	ca, err := h.mgr.CreateCronAgent(r.Context(), req.toCronAgentCreateSpec())
	if err != nil {
		h.cronAgentError(w, "creating cron agent", err)
		return
	}
	w.Header().Set("Location", "/api/cron-agents/"+ca.ID)
	writeJSON(w, http.StatusCreated, ca)
}

func (h *Handler) handleUpdateCronAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req cronAgentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	if ae := validateCronAgentFields(req); ae != nil {
		ae.write(w)
		return
	}
	if !h.lookUpTemplateForCronAgent(w, r, req.TemplateID) {
		return
	}

	ca, err := h.mgr.UpdateCronAgent(r.Context(), id, req.toCronAgentCreateSpec())
	if err != nil {
		h.cronAgentError(w, "updating cron agent "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, ca)
}

// handleDeleteCronAgent always answers 200, whether or not the cron agent
// existed, matching handleDeleteTemplate's idempotent stance.
func (h *Handler) handleDeleteCronAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.mgr.DeleteCronAgent(r.Context(), id); err != nil {
		h.internalError(w, "deleting cron agent "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "deleted"})
}

func (h *Handler) handleListCronAgentScheduleRuns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	runs, err := h.mgr.ListScheduleRunsByCronAgent(r.Context(), id)
	if err != nil {
		h.internalError(w, "listing schedule runs for cron agent "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, scheduleRunListResponse{Runs: runs})
}

// scheduleRunListResponse is the GET /api/cron-agents/{id}/runs body, same
// wrapped-array convention as cronAgentListResponse. Moved here from
// templates.go now that run history belongs to CronAgent, not Template.
type scheduleRunListResponse struct {
	Runs []store.ScheduleRun `json:"runs"`
}
