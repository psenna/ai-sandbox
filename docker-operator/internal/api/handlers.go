package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/filestore"
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

	// PurgeAgentFiles removes the agent's directory in the centralized file
	// store. A no-op when the store is disabled; idempotent. Called by
	// handleDelete only when ?purge_files=true.
	PurgeAgentFiles(ctx context.Context, id string) error

	Get(ctx context.Context, id string) (store.Agent, error)
	List(ctx context.Context) ([]store.Agent, error)
	MaxAgents() int
	Rename(ctx context.Context, id string, name, description *string) (store.Agent, error)

	// Update recreates an agent's container in place (a new image tag, or a
	// changed create-form field) under the same agent ID and the same
	// volumes. Errors: store.IsNotFound (404), agent.IsNotUpdatable (409),
	// agent.IsInvalidImageTag (400), agent.IsNoAnthropicAuth (409), and the
	// invalid-backend/ollama/repo/threshold family (400).
	Update(ctx context.Context, id string, req agent.UpdateRequest) (store.Agent, error)

	// DefaultBackend/DefaultModel/DefaultFastModel/DefaultOllamaURL/DefaultRepo
	// are the operator-configured defaults the create form pre-fills; they
	// ride along on the list response so the UI needs no second request.
	// DefaultRepo is "" when the operator configured no GITHUB_REPO;
	// DefaultOllamaURL is "" when the operator cleared OLLAMA_URL.
	DefaultBackend() string
	DefaultModel() string
	DefaultFastModel() string
	DefaultOllamaURL() string
	DefaultRepo() string
	DefaultAutoCompactThreshold() string
	DefaultMaxContextTokens() string
	// DefaultAutoMode is config.AutoModeOn/AutoModeOff, resolved from the
	// operator's AGENT_AUTO_MODE; the create form shows it as what "operator
	// default" resolves to.
	DefaultAutoMode() string

	// AgentImage/DockerRuntime are the operator-level parameters every agent
	// inherits (its container image, its DinD sidecar's runtime); the
	// /api/agents/{id}/info handler serves them next to the agent record.
	AgentImage() string
	DockerRuntime() string

	// AgentImageTags returns the operator's last-known snapshot of the agent
	// image's published tags (the bool is false before the first refresh
	// completes); RefreshAgentImageTags forces a poll now. A poll error is
	// non-fatal to the caller -- the last-known list is kept.
	AgentImageTags(ctx context.Context) (store.AgentImageTags, bool, error)
	RefreshAgentImageTags(ctx context.Context) error

	// AnthropicAuthStatus reports whether a shared Anthropic credential is
	// configured, its kind and when it was last set -- never its value.
	// SetAnthropicAuth stores (replacing) it; ClearAnthropicAuth removes it
	// (idempotent).
	AnthropicAuthStatus(ctx context.Context) (kind string, updatedAt time.Time, configured bool, err error)
	SetAnthropicAuth(ctx context.Context, kind, value string) error
	ClearAnthropicAuth(ctx context.Context) error

	// StartAnthropicLogin ensures the singleton `claude setup-token` helper
	// container is running (idempotent); StopAnthropicLogin tears it down
	// (idempotent); AnthropicLoginActive reports whether it exists.
	StartAnthropicLogin(ctx context.Context) error
	StopAnthropicLogin(ctx context.Context) error
	AnthropicLoginActive(ctx context.Context) (bool, error)
}

// anthropicLoginWSPath is the WebSocket route cmd/docker-operator wires to
// wsbridge.NewContainerTerminalHandler for the login helper. Returned to the
// UI by the login endpoints so it does not hard-code the path.
const anthropicLoginWSPath = "/ws/anthropic/login/terminal"

// Handler serves the docker-operator's REST surface: the agent collection
// and item endpoints, and the output endpoint.
type Handler struct {
	mgr    AgentManager
	docker dockerclient.ExecClient
	log    *slog.Logger
	// files is the centralized file store, or nil when it is disabled -- in
	// which case every /api/files* route answers 501 filestore_disabled.
	files *filestore.Store
	// maxUpload caps a single uploaded file (POST /api/files/upload).
	maxUpload int64
}

// NewHandler builds the docker-operator's HTTP handler. mgr is the agent
// lifecycle (create/delete/list/rename); docker is used only for the output
// endpoint's exec into a running agent container -- the narrowest interface
// that works, matching internal/wsbridge.ReadOutput's own signature. files is
// the centralized file store (nil disables the /api/files* routes, which then
// answer 501); maxUpload is the per-file upload cap. A nil log falls back to
// slog.Default.
func NewHandler(mgr AgentManager, docker dockerclient.ExecClient, files *filestore.Store, maxUpload int64, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	h := &Handler{mgr: mgr, docker: docker, log: log, files: files, maxUpload: maxUpload}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/agents", h.handleList)
	mux.HandleFunc("POST /api/agents", h.handleCreate)
	mux.HandleFunc("GET /api/agents/{id}", h.handleGet)
	mux.HandleFunc("PATCH /api/agents/{id}", h.handleRename)
	mux.HandleFunc("DELETE /api/agents/{id}", h.handleDelete)
	mux.HandleFunc("POST /api/agents/{id}/update", h.handleUpdate)
	mux.HandleFunc("GET /api/agents/{id}/output", h.handleOutput)
	mux.HandleFunc("GET /api/agents/{id}/info", h.handleAgentInfo)

	mux.HandleFunc("GET /api/files", h.handleFilesList)
	mux.HandleFunc("DELETE /api/files", h.handleFilesDelete)
	mux.HandleFunc("GET /api/files/download", h.handleFilesDownload)
	mux.HandleFunc("POST /api/files/upload", h.handleFilesUpload)
	mux.HandleFunc("POST /api/files/mkdir", h.handleFilesMkdir)

	mux.HandleFunc("GET /api/agent-image/tags", h.handleAgentImageTags)
	mux.HandleFunc("POST /api/agent-image/refresh", h.handleAgentImageRefresh)

	mux.HandleFunc("GET /api/anthropic/auth", h.handleAnthropicAuthGet)
	mux.HandleFunc("PUT /api/anthropic/auth", h.handleAnthropicAuthPut)
	mux.HandleFunc("DELETE /api/anthropic/auth", h.handleAnthropicAuthDelete)
	mux.HandleFunc("GET /api/anthropic/login", h.handleAnthropicLoginGet)
	mux.HandleFunc("POST /api/anthropic/login", h.handleAnthropicLoginStart)
	mux.HandleFunc("DELETE /api/anthropic/login", h.handleAnthropicLoginStop)

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
	mux.HandleFunc("/api/agents/{id}/update", methodNotAllowed)
	mux.HandleFunc("/api/agents/{id}/output", methodNotAllowed)
	mux.HandleFunc("/api/agents/{id}/info", methodNotAllowed)
	mux.HandleFunc("/api/agent-image/tags", methodNotAllowed)
	mux.HandleFunc("/api/agent-image/refresh", methodNotAllowed)
	mux.HandleFunc("/api/anthropic/auth", methodNotAllowed)
	mux.HandleFunc("/api/anthropic/login", methodNotAllowed)
	mux.HandleFunc("/api/files", methodNotAllowed)
	mux.HandleFunc("/api/files/download", methodNotAllowed)
	mux.HandleFunc("/api/files/upload", methodNotAllowed)
	mux.HandleFunc("/api/files/mkdir", methodNotAllowed)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path, "")
	})

	return mux
}

// --- request/response DTOs -------------------------------------------------

// createAgentRequest is the POST /api/agents body. Every field is optional:
// name/description default to empty (fill them in later via PATCH); backend
// defaults to the operator's DefaultBackend; model/fast_model default to the
// operator's configured Ollama models and are only valid for the ollama
// backend.
type createAgentRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Backend     string `json:"backend"`
	Model       string `json:"model"`
	FastModel   string `json:"fast_model"`
	// OllamaURL overrides the operator's OLLAMA_URL for this one agent (the
	// Ollama server its model traffic is routed to). Empty falls back to that
	// default; only valid for the ollama backend.
	OllamaURL string `json:"ollama_url"`
	// Repo is this agent's owner/repo.git, overriding the operator's
	// GITHUB_REPO default. Empty falls back to that default; empty with no
	// default means the agent boots as a bare terminal. Nothing clones it.
	Repo string `json:"repo"`
	// AutoCompactThreshold is this agent's Claude Code auto-compact threshold,
	// templated into its environment as CLAUDE_AUTO_COMPACT_THRESHOLD. Backend-
	// agnostic. Empty falls back to the operator's default; empty with no
	// default means the variable is omitted so the agent uses Claude Code's
	// built-in default. When non-empty it must be an integer between 50 and
	// 100.
	AutoCompactThreshold string `json:"auto_compact_threshold"`
	// MaxContextTokens is this agent's Claude Code max-context token budget,
	// templated into its environment as CLAUDE_CODE_MAX_CONTEXT_TOKENS.
	// Backend-agnostic. Empty falls back to the operator's default; empty with
	// no default means the variable is omitted so the agent uses Claude Code's
	// built-in default.
	MaxContextTokens string `json:"max_context_tokens"`
	// ImageTag pins this one agent to a specific tag of the operator's agent
	// image repository. Empty means the operator's configured image. A
	// malformed tag is rejected with 400 on the "image_tag" field.
	ImageTag string `json:"image_tag"`
	// AutoMode overrides the operator's AGENT_AUTO_MODE default for this one
	// agent's `claude` process: "on", "off", or "" to use that default.
	// Backend-agnostic. An unrecognised non-empty value is rejected with 400
	// on the "auto_mode" field.
	AutoMode string `json:"auto_mode"`
}

// updateAgentRequest is the POST /api/agents/{id}/update body: every
// createAgentRequest field (all editable in place) plus the image tag. It
// shares createAgentRequest's field validation and DTO mapping with
// handleCreate via validateAgentFields / toCreateRequest.
type updateAgentRequest struct {
	createAgentRequest
	ImageTag string `json:"image_tag"`
}

// apiErr is a deferred writeError call: the create/update field validation
// builds one instead of writing straight to the ResponseWriter, so the same
// checks serve both handlers.
type apiErr struct {
	status int
	code   string
	msg    string
	field  string
}

func (e *apiErr) write(w http.ResponseWriter) { writeError(w, e.status, e.code, e.msg, e.field) }

// validateAgentFields runs the create-form field checks shared by
// handleCreate and handleUpdate. It returns nil when every field is
// acceptable, or the (unwritten) error otherwise.
func validateAgentFields(req createAgentRequest) *apiErr {
	switch {
	case req.Backend != "" && !config.ValidBackend(req.Backend):
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"backend" must be "ollama" or "anthropic"`, "backend"}
	case req.Backend == config.BackendAnthropic && (req.Model != "" || req.FastModel != "" || req.OllamaURL != ""):
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"model", "fast_model" and "ollama_url" are not valid for the anthropic backend`, "model"}
	case req.OllamaURL != "" && !config.ValidOllamaURL(req.OllamaURL):
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"ollama_url" must be an http or https URL`, "ollama_url"}
	case req.Repo != "" && !config.ValidGithubRepo(req.Repo):
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"repo" must be "owner/repo" or "owner/repo.git"`, "repo"}
	case req.AutoCompactThreshold != "" && !config.ValidAutoCompactThreshold(req.AutoCompactThreshold):
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"auto_compact_threshold" must be an integer between 50 and 100`, "auto_compact_threshold"}
	case req.AutoMode != "" && !config.ValidAutoMode(req.AutoMode):
		return &apiErr{http.StatusBadRequest, CodeInvalidParam, `"auto_mode" must be "on", "off" or omitted`, "auto_mode"}
	default:
		return nil
	}
}

// toCreateRequest maps the validated DTO to the internal agent.CreateRequest.
func toCreateRequest(req createAgentRequest) agent.CreateRequest {
	return agent.CreateRequest{
		Name: req.Name, Description: req.Description,
		Backend: req.Backend, Model: req.Model, FastModel: req.FastModel,
		OllamaURL: req.OllamaURL, Repo: req.Repo,
		AutoCompactThreshold: req.AutoCompactThreshold,
		MaxContextTokens:     req.MaxContextTokens,
		ImageTag:             req.ImageTag,
		AutoMode:             req.AutoMode,
	}
}

// patchAgentRequest is the PATCH /api/agents/{id} body. A nil field leaves
// the corresponding value unchanged; a present field (including an explicit
// empty string, e.g. clearing a description) sets it. At least one of the
// two must be present.
type patchAgentRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// agentView is a store.Agent plus the computed, per-request UpgradeAvailable
// flag. The embedded struct makes encoding/json flatten every store.Agent
// field to the top level and append "upgrade_available" alongside them, so
// existing clients that only read id/name/... keep working unchanged.
type agentView struct {
	store.Agent
	// UpgradeAvailable is true when the agent runs a date-time image tag and
	// the operator's discovered tag list contains a strictly newer one. It is
	// false for an agent on :latest, on any non-date-time tag, with no
	// recorded image, or when the tag list is unavailable.
	UpgradeAvailable bool `json:"upgrade_available"`
}

// buildAgentViews wraps each agent in an agentView, computing
// UpgradeAvailable against the operator's last-known agent-image tag list.
// The tag list is fetched once and best-effort: on any error, or before the
// first refresh completes, it is treated as empty and nothing is flagged --
// the list must not fail because tag discovery is unavailable. The result is
// non-nil even for an empty input.
func (h *Handler) buildAgentViews(ctx context.Context, agents []store.Agent) []agentView {
	var tags []string
	if snap, ok, err := h.mgr.AgentImageTags(ctx); err == nil && ok {
		tags = snap.Tags
	}
	views := make([]agentView, 0, len(agents))
	for _, a := range agents {
		views = append(views, agentView{
			Agent:            a,
			UpgradeAvailable: agent.UpgradeAvailable(agent.ImageTagOf(a.Image), tags),
		})
	}
	return views
}

// agentListResponse is the GET /api/agents body. MaxAgents and the three
// Default* fields ride along so the UI can render "3 of 5 slots in use" and
// pre-fill the create form without a second request.
type agentListResponse struct {
	Agents           []agentView `json:"agents"`
	MaxAgents        int         `json:"max_agents"`
	DefaultBackend   string      `json:"default_backend"`
	DefaultModel     string      `json:"default_model"`
	DefaultFastModel string      `json:"default_fast_model"`
	DefaultOllamaURL string      `json:"default_ollama_url"`
	DefaultRepo      string      `json:"default_repo"`
	// DefaultAutoCompactThreshold is "" when the operator set no
	// AGENT_AUTO_COMPACT_THRESHOLD; the UI then shows a blank field meaning
	// "the agent uses Claude Code's built-in default".
	DefaultAutoCompactThreshold string `json:"default_auto_compact_threshold"`
	// DefaultMaxContextTokens is "" when the operator set no
	// AGENT_MAX_CONTEXT_TOKENS; the UI then shows a blank field meaning
	// "the agent uses Claude Code's built-in default".
	DefaultMaxContextTokens string `json:"default_max_context_tokens"`
	// DefaultAutoMode is config.AutoModeOn or config.AutoModeOff, resolved
	// from the operator's AGENT_AUTO_MODE; the create form shows it as what
	// leaving its "Auto mode" field on "operator default" resolves to.
	DefaultAutoMode string `json:"default_auto_mode"`
}

// agentOperatorInfo is the operator-level part of the /api/agents/{id}/info
// response: the parameters set on the operator (not per agent) that apply to
// every agent -- its container image and the DinD sidecar's runtime.
type agentOperatorInfo struct {
	AgentImage    string `json:"agent_image"`
	DockerRuntime string `json:"docker_runtime"`
}

// agentInfoResponse is the GET /api/agents/{id}/info body: the agent record
// (its resolved create-time parameters) alongside the operator defaults the
// UI's "Agent info" overlay renders in a second section.
type agentInfoResponse struct {
	Agent    store.Agent       `json:"agent"`
	Operator agentOperatorInfo `json:"operator"`
}

// agentImageTagsResponse is the GET /api/agent-image/tags and
// POST /api/agent-image/refresh body: the discovered date-time tags
// (newest-first), the newest of them, the operator's own default image tag,
// when the list was last checked, and the last refresh error if any.
type agentImageTagsResponse struct {
	Tags            []string   `json:"tags"`
	Newest          string     `json:"newest"`
	OperatorDefault string     `json:"operator_default"`
	CheckedAt       *time.Time `json:"checked_at"`
	LastError       string     `json:"last_error"`
}

// anthropicAuthRequest is the PUT /api/anthropic/auth body.
type anthropicAuthRequest struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// anthropicAuthResponse is the GET/PUT/DELETE /api/anthropic/auth body. It
// never carries the credential value -- only whether one is configured, its
// kind, and when it was last set.
type anthropicAuthResponse struct {
	Configured bool       `json:"configured"`
	Kind       string     `json:"kind"`
	UpdatedAt  *time.Time `json:"updated_at"`
}

// anthropicLoginResponse is the GET/POST/DELETE /api/anthropic/login body.
// When Active, WS is the path the UI opens a terminal WebSocket to.
type anthropicLoginResponse struct {
	Active bool   `json:"active"`
	WS     string `json:"ws,omitempty"`
}

// --- handlers ---------------------------------------------------------------

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	agents, err := h.mgr.List(r.Context())
	if err != nil {
		h.internalError(w, "listing agents", err)
		return
	}
	writeJSON(w, http.StatusOK, agentListResponse{
		Agents:                      h.buildAgentViews(r.Context(), agents),
		MaxAgents:                   h.mgr.MaxAgents(),
		DefaultBackend:              h.mgr.DefaultBackend(),
		DefaultModel:                h.mgr.DefaultModel(),
		DefaultFastModel:            h.mgr.DefaultFastModel(),
		DefaultOllamaURL:            h.mgr.DefaultOllamaURL(),
		DefaultRepo:                 h.mgr.DefaultRepo(),
		DefaultAutoCompactThreshold: h.mgr.DefaultAutoCompactThreshold(),
		DefaultMaxContextTokens:     h.mgr.DefaultMaxContextTokens(),
		DefaultAutoMode:             h.mgr.DefaultAutoMode(),
	})
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createAgentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	if ae := validateAgentFields(req); ae != nil {
		ae.write(w)
		return
	}

	a, err := h.mgr.Create(r.Context(), toCreateRequest(req))
	if err != nil {
		switch {
		case store.IsAtCapacity(err):
			writeError(w, http.StatusConflict, CodeAtCapacity, "the maximum number of agents is already running; delete one before creating another", "")
		case agent.IsNoAnthropicAuth(err):
			writeError(w, http.StatusConflict, CodeNoAnthropicAuth, "configure the Anthropic account (PUT /api/anthropic/auth) before creating an agent that uses it", "backend")
		case agent.IsInvalidBackend(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"backend" must be "ollama" or "anthropic"`, "backend")
		case agent.IsInvalidOllamaURL(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"ollama_url" must be an http or https URL`, "ollama_url")
		case agent.IsInvalidRepo(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"repo" must be "owner/repo" or "owner/repo.git"`, "repo")
		case agent.IsInvalidAutoCompactThreshold(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"auto_compact_threshold" must be an integer between 50 and 100`, "auto_compact_threshold")
		case agent.IsInvalidAutoMode(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"auto_mode" must be "on", "off" or omitted`, "auto_mode")
		case agent.IsInvalidImageTag(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"image_tag" is not a valid image tag`, "image_tag")
		default:
			h.internalError(w, "creating agent", err)
		}
		return
	}
	w.Header().Set("Location", "/api/agents/"+a.ID)
	writeJSON(w, http.StatusCreated, a)
}

// agentImageTagsBody builds the response DTO from a stored snapshot.
func (h *Handler) agentImageTagsBody(info store.AgentImageTags) agentImageTagsResponse {
	tags := info.Tags
	if tags == nil {
		tags = []string{}
	}
	resp := agentImageTagsResponse{
		Tags:            tags,
		Newest:          agent.NewestDateTimeTag(info.Tags),
		OperatorDefault: agent.ImageTagOf(h.mgr.AgentImage()),
		LastError:       info.LastError,
	}
	if !info.CheckedAt.IsZero() {
		t := info.CheckedAt
		resp.CheckedAt = &t
	}
	return resp
}

func (h *Handler) handleAgentImageTags(w http.ResponseWriter, r *http.Request) {
	info, _, err := h.mgr.AgentImageTags(r.Context())
	if err != nil {
		h.internalError(w, "reading the agent image tags", err)
		return
	}
	writeJSON(w, http.StatusOK, h.agentImageTagsBody(info))
}

func (h *Handler) handleAgentImageRefresh(w http.ResponseWriter, r *http.Request) {
	// A poll failure is not fatal to this request: the refresh keeps the
	// last-known list and records the error, and the client still gets a 200
	// with last_error populated.
	if err := h.mgr.RefreshAgentImageTags(r.Context()); err != nil {
		h.log.Warn("on-demand agent image tag refresh failed", "error", err)
	}
	info, _, err := h.mgr.AgentImageTags(r.Context())
	if err != nil {
		h.internalError(w, "reading back the agent image tags", err)
		return
	}
	writeJSON(w, http.StatusOK, h.agentImageTagsBody(info))
}

func (h *Handler) handleAnthropicAuthGet(w http.ResponseWriter, r *http.Request) {
	kind, updatedAt, configured, err := h.mgr.AnthropicAuthStatus(r.Context())
	if err != nil {
		h.internalError(w, "reading the Anthropic credential status", err)
		return
	}
	writeJSON(w, http.StatusOK, anthropicAuthStatusBody(kind, updatedAt, configured))
}

func (h *Handler) handleAnthropicAuthPut(w http.ResponseWriter, r *http.Request) {
	var req anthropicAuthRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	if !store.ValidAnthropicKind(req.Kind) {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, `"kind" must be "api_key" or "oauth"`, "kind")
		return
	}
	// A credential pasted from a terminal (the setup-token helper's tmux pane,
	// a shell) almost always arrives with a trailing newline, and an 80-column
	// pane can hard-wrap the ~100-char OAuth token so the paste has a newline
	// mid-string. Neither survives as a usable bearer: it is injected verbatim
	// as CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY, Claude Code sends the
	// mangled value, the API 401s, and the agent silently drops to an
	// interactive login. Trim the outside; reject interior whitespace (no valid
	// Anthropic key or token contains any) rather than store a value that
	// cannot work.
	req.Value = strings.TrimSpace(req.Value)
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, CodeMissingField, `"value" must not be empty`, "value")
		return
	}
	if strings.ContainsFunc(req.Value, unicode.IsSpace) {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, `"value" must not contain whitespace (a wrapped or truncated paste?)`, "value")
		return
	}
	// Cheap shape checks. Both credentials go into every anthropic-backend
	// agent's environment verbatim (ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN),
	// where a truncated or wrong-field paste does not error loudly -- Claude Code
	// just ignores it and drops the agent to an interactive login. Catching the
	// obvious mistakes here is worth the brittleness of a prefix match.
	//   - a Console API key starts with "sk-ant-"
	//   - a `claude setup-token` OAuth token starts with "sk-ant-oat01-"
	switch req.Kind {
	case store.AnthropicKindAPIKey:
		if !strings.HasPrefix(req.Value, "sk-ant-") {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `an Anthropic API key starts with "sk-ant-"`, "value")
			return
		}
	case store.AnthropicKindOAuth:
		if !strings.HasPrefix(req.Value, "sk-ant-oat01-") {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `a Claude Code OAuth token (from "claude setup-token") starts with "sk-ant-oat01-"`, "value")
			return
		}
	}

	if err := h.mgr.SetAnthropicAuth(r.Context(), req.Kind, req.Value); err != nil {
		h.internalError(w, "storing the Anthropic credential", err)
		return
	}
	// The credential is now stored, so a running `claude setup-token` helper
	// has done its job -- tear it down. Best-effort: a failure here does not
	// undo the store, so the request still succeeded.
	if err := h.mgr.StopAnthropicLogin(r.Context()); err != nil {
		h.log.Warn("could not tear down the Anthropic-login helper after storing the credential", "error", err)
	}
	kind, updatedAt, configured, err := h.mgr.AnthropicAuthStatus(r.Context())
	if err != nil {
		h.internalError(w, "reading back the Anthropic credential status", err)
		return
	}
	writeJSON(w, http.StatusOK, anthropicAuthStatusBody(kind, updatedAt, configured))
}

func (h *Handler) handleAnthropicAuthDelete(w http.ResponseWriter, r *http.Request) {
	if err := h.mgr.ClearAnthropicAuth(r.Context()); err != nil {
		h.internalError(w, "clearing the Anthropic credential", err)
		return
	}
	writeJSON(w, http.StatusOK, anthropicAuthResponse{Configured: false})
}

func (h *Handler) handleAnthropicLoginGet(w http.ResponseWriter, r *http.Request) {
	active, err := h.mgr.AnthropicLoginActive(r.Context())
	if err != nil {
		h.internalError(w, "checking the Anthropic-login helper", err)
		return
	}
	writeJSON(w, http.StatusOK, anthropicLoginBody(active))
}

func (h *Handler) handleAnthropicLoginStart(w http.ResponseWriter, r *http.Request) {
	if err := h.mgr.StartAnthropicLogin(r.Context()); err != nil {
		h.internalError(w, "starting the Anthropic-login helper", err)
		return
	}
	writeJSON(w, http.StatusOK, anthropicLoginBody(true))
}

func (h *Handler) handleAnthropicLoginStop(w http.ResponseWriter, r *http.Request) {
	if err := h.mgr.StopAnthropicLogin(r.Context()); err != nil {
		h.internalError(w, "stopping the Anthropic-login helper", err)
		return
	}
	writeJSON(w, http.StatusOK, anthropicLoginBody(false))
}

func anthropicLoginBody(active bool) anthropicLoginResponse {
	resp := anthropicLoginResponse{Active: active}
	if active {
		resp.WS = anthropicLoginWSPath
	}
	return resp
}

func anthropicAuthStatusBody(kind string, updatedAt time.Time, configured bool) anthropicAuthResponse {
	resp := anthropicAuthResponse{Configured: configured, Kind: kind}
	if configured {
		u := updatedAt
		resp.UpdatedAt = &u
	}
	return resp
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := h.mgr.Get(r.Context(), id)
	if err != nil {
		h.notFoundOrInternal(w, "getting agent "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, h.buildAgentViews(r.Context(), []store.Agent{a})[0])
}

// handleAgentInfo serves GET /api/agents/{id}/info: the agent record (which
// carries its resolved create-time parameters) plus the operator-level
// parameters that apply to every agent. It is the data source for the UI's
// "Agent info" overlay -- one request instead of the client combining the
// item endpoint with a config endpoint that does not exist.
func (h *Handler) handleAgentInfo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := h.mgr.Get(r.Context(), id)
	if err != nil {
		h.notFoundOrInternal(w, "getting agent "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, agentInfoResponse{
		Agent: a,
		Operator: agentOperatorInfo{
			AgentImage:    h.mgr.AgentImage(),
			DockerRuntime: h.mgr.DockerRuntime(),
		},
	})
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

// handleUpdate serves POST /api/agents/{id}/update: recreate the agent's
// container in place (a new image tag, or any changed create-form field)
// under the same agent ID and the same volumes. The running tmux/claude
// session ends; the new container resumes it via `claude --continue`.
func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req updateAgentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	if ae := validateAgentFields(req.createAgentRequest); ae != nil {
		ae.write(w)
		return
	}

	a, err := h.mgr.Update(r.Context(), id, agent.UpdateRequest{
		CreateRequest: toCreateRequest(req.createAgentRequest),
		ImageTag:      req.ImageTag,
	})
	if err != nil {
		switch {
		case store.IsNotFound(err):
			writeError(w, http.StatusNotFound, CodeNotFound, "no such agent", "")
		case agent.IsNotUpdatable(err):
			writeError(w, http.StatusConflict, CodeNotUpdatable, "the agent is not in an updatable state (only running, stopped or error agents can be updated)", "")
		case agent.IsInvalidImageTag(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"image_tag" is not a valid image tag`, "image_tag")
		case agent.IsNoAnthropicAuth(err):
			writeError(w, http.StatusConflict, CodeNoAnthropicAuth, "configure the Anthropic account (PUT /api/anthropic/auth) before switching an agent to it", "backend")
		case agent.IsInvalidBackend(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"backend" must be "ollama" or "anthropic"`, "backend")
		case agent.IsInvalidOllamaURL(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"ollama_url" must be an http or https URL`, "ollama_url")
		case agent.IsInvalidRepo(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"repo" must be "owner/repo" or "owner/repo.git"`, "repo")
		case agent.IsInvalidAutoCompactThreshold(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"auto_compact_threshold" must be an integer between 50 and 100`, "auto_compact_threshold")
		case agent.IsInvalidAutoMode(err):
			writeError(w, http.StatusBadRequest, CodeInvalidParam, `"auto_mode" must be "on", "off" or omitted`, "auto_mode")
		default:
			h.internalError(w, "updating agent "+id, err)
		}
		return
	}
	// Wrapped in an agentView for consistency with handleGet, so the client
	// gets the same shape (with upgrade_available) it reads elsewhere.
	writeJSON(w, http.StatusOK, h.buildAgentViews(r.Context(), []store.Agent{a})[0])
}

// handleDelete always answers 200, whether or not the agent existed:
// agent.Manager.Delete is itself idempotent (removing an already-gone agent
// is success, the same convention every dockerclient remove call uses), so a
// second DELETE on the same id is not an error -- the caller's desired end
// state (no such agent) already holds.
func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	purge := false
	if raw := r.URL.Query().Get("purge_files"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, "purge_files must be a boolean, got "+strconv.Quote(raw), "purge_files")
			return
		}
		purge = v
	}

	if err := h.mgr.Delete(r.Context(), id); err != nil {
		h.internalError(w, "deleting agent "+id, err)
		return
	}
	// Purge AFTER the agent is torn down, and only when asked: the agent's
	// files in the centralized store otherwise survive a delete by design.
	if purge {
		if err := h.mgr.PurgeAgentFiles(r.Context(), id); err != nil {
			h.internalError(w, "purging files for agent "+id, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "deleted", "files_purged": purge})
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
