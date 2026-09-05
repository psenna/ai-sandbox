package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
	"github.com/psenna/ai-sandbox/docker-operator/internal/wsbridge"
)

// AgentManager is the entire agent-lifecycle surface these handlers depend
// on. *agent.Manager satisfies it; a fake satisfying it is what the
// acceptance criteria's "httptest-based handler unit tests against a fake
// agent manager" tests against.
//
// Reading (Get/List/MaxAgents) and renaming never touch Docker -- they are
// thin passes to internal/store -- but living on this one interface, rather
// than internal/api depending on *agent.Manager for Create/Delete and
// *store.Store for everything else, keeps these handlers dependent on
// exactly one thing.
type AgentManager interface {
	Create(ctx context.Context, req agent.CreateRequest) (store.Agent, error)
	Delete(ctx context.Context, id string) error
	Get(ctx context.Context, id string) (store.Agent, error)
	List(ctx context.Context) ([]store.Agent, error)
	MaxAgents() int
	Rename(ctx context.Context, id string, name, description *string) (store.Agent, error)
}

// Handler serves the docker-operator's REST surface: the agent collection
// and item endpoints, and the output endpoint.
type Handler struct {
	mgr    AgentManager
	docker dockerclient.ExecClient
	log    *slog.Logger
}

// NewHandler builds the docker-operator's HTTP handler. mgr is the agent
// lifecycle (create/delete/list/rename); docker is used only for the output
// endpoint's exec into a running agent container -- the narrowest interface
// that works, matching internal/wsbridge.ReadOutput's own signature. A nil
// log falls back to slog.Default.
func NewHandler(mgr AgentManager, docker dockerclient.ExecClient, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	h := &Handler{mgr: mgr, docker: docker, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/agents", h.handleList)
	mux.HandleFunc("POST /api/agents", h.handleCreate)
	mux.HandleFunc("GET /api/agents/{id}", h.handleGet)
	mux.HandleFunc("PATCH /api/agents/{id}", h.handleRename)
	mux.HandleFunc("DELETE /api/agents/{id}", h.handleDelete)
	mux.HandleFunc("GET /api/agents/{id}/output", h.handleOutput)

	// Bare, method-agnostic registrations for the same paths: net/http's
	// ServeMux (1.22+) prefers a method-specific pattern for a matching
	// method, but falls back to these for any OTHER method on the same
	// path -- giving a real 405, not a silent 404 fall-through to the
	// catch-all below. Matches the pattern operator/internal/sandboxctl's
	// server.go already established in this repo.
	methodNotAllowed := func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method "+r.Method+" is not allowed on "+r.URL.Path, "")
	}
	mux.HandleFunc("/api/agents", methodNotAllowed)
	mux.HandleFunc("/api/agents/{id}", methodNotAllowed)
	mux.HandleFunc("/api/agents/{id}/output", methodNotAllowed)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path, "")
	})

	return mux
}

// --- request/response DTOs -------------------------------------------------

// createAgentRequest is the POST /api/agents body. Both fields are optional
// (an agent with no name/description is valid -- the UI can fill them in
// later via PATCH).
type createAgentRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// patchAgentRequest is the PATCH /api/agents/{id} body. A nil field leaves
// the corresponding value unchanged; a present field (including an explicit
// empty string, e.g. clearing a description) sets it. At least one of the
// two must be present.
type patchAgentRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// agentListResponse is the GET /api/agents body. MaxAgents rides along so
// the UI can render "3 of 5 slots in use" without a second request.
type agentListResponse struct {
	Agents    []store.Agent `json:"agents"`
	MaxAgents int           `json:"max_agents"`
}

// --- handlers ---------------------------------------------------------------

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	agents, err := h.mgr.List(r.Context())
	if err != nil {
		h.internalError(w, "listing agents", err)
		return
	}
	if agents == nil {
		agents = []store.Agent{}
	}
	writeJSON(w, http.StatusOK, agentListResponse{Agents: agents, MaxAgents: h.mgr.MaxAgents()})
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createAgentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}

	a, err := h.mgr.Create(r.Context(), agent.CreateRequest{Name: req.Name, Description: req.Description})
	if err != nil {
		switch {
		case store.IsAtCapacity(err):
			writeError(w, http.StatusConflict, CodeAtCapacity, "the maximum number of agents is already running; delete one before creating another", "")
		default:
			h.internalError(w, "creating agent", err)
		}
		return
	}
	w.Header().Set("Location", "/api/agents/"+a.ID)
	writeJSON(w, http.StatusCreated, a)
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := h.mgr.Get(r.Context(), id)
	if err != nil {
		h.notFoundOrInternal(w, "getting agent "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) handleRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req patchAgentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	if req.Name == nil && req.Description == nil {
		writeError(w, http.StatusBadRequest, CodeMissingField,
			"provide at least one of \"name\" or \"description\" to update", "")
		return
	}

	a, err := h.mgr.Rename(r.Context(), id, req.Name, req.Description)
	if err != nil {
		h.notFoundOrInternal(w, "renaming agent "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// handleDelete always answers 200, whether or not the agent existed:
// agent.Manager.Delete is itself idempotent (removing an already-gone agent
// is success, the same convention every dockerclient remove call uses), so a
// second DELETE on the same id is not an error -- the caller's desired end
// state (no such agent) already holds.
func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.mgr.Delete(r.Context(), id); err != nil {
		h.internalError(w, "deleting agent "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "deleted"})
}

// handleOutput serves GET /api/agents/{id}/output?tail=N as raw bytes
// (Content-Type: text/plain), not JSON: internal/wsbridge.ReadOutput
// explicitly returns []byte because a pane's captured output is not
// guaranteed to be valid UTF-8, and JSON-string-escaping arbitrary terminal
// bytes (control codes, partial multi-byte sequences) would be lossy in
// exactly the cases that matter most for debugging. A browser or curl asking
// for a log tail wants the bytes, not an escaped wrapper around them.
func (h *Handler) handleOutput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	tail := wsbridge.TailAll
	if raw := r.URL.Query().Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, "tail must be an integer, got "+strconv.Quote(raw), "tail")
			return
		}
		tail = n
	}

	a, err := h.mgr.Get(r.Context(), id)
	if err != nil {
		h.notFoundOrInternal(w, "getting agent "+id, err)
		return
	}

	// No container yet (the agent is still StatusCreating, or -- honestly --
	// something went wrong before one was ever stamped): there is nothing to
	// exec into. This is the same "no output captured yet is not an error"
	// stance ReadOutput itself takes for a missing log file, extended one
	// layer out to "no container to look for a log file in".
	if a.ContainerID == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}

	out, err := wsbridge.ReadOutput(r.Context(), h.docker, a.ContainerID, tail)
	if err != nil {
		switch {
		case dockerclient.IsNotFound(err):
			writeError(w, http.StatusNotFound, CodeNotFound,
				"agent "+id+"'s container is gone; it may have been stopped or removed outside the operator", "")
		default:
			h.internalError(w, "reading output for agent "+id, err)
		}
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// --- error mapping helpers ---------------------------------------------------

// notFoundOrInternal maps store.IsNotFound to 404 and anything else to 500,
// the shape every id-scoped handler above needs.
func (h *Handler) notFoundOrInternal(w http.ResponseWriter, action string, err error) {
	if store.IsNotFound(err) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such agent", "")
		return
	}
	h.internalError(w, action, err)
}

// internalError logs the real error (which may name internal resource
// details not meant for the response) and writes a generic 500.
func (h *Handler) internalError(w http.ResponseWriter, action string, err error) {
	h.log.Error(action+" failed", "error", err)
	writeError(w, http.StatusInternalServerError, CodeInternal, "internal error", "")
}

// decodeJSON decodes r's body into v, rejecting unknown fields and a
// trailing second JSON value -- the same strictness convention
// operator/internal/sandboxctl's decodeStrict uses. An empty body decodes to
// the zero value of v (every request body in this API is optional: an empty
// createAgentRequest is a valid, unnamed agent).
func decodeJSON(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errTrailingData
	}
	return nil
}

// errTrailingData is decodeJSON's error for a body carrying more than one
// JSON value.
var errTrailingData = errors.New("body must contain a single JSON value")
