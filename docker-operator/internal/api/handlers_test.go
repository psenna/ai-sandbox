package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// fakeManager is a small in-memory AgentManager for handler tests. It is
// deliberately much lighter than dockerclienttest.Fake: this interface has
// six methods and no concurrent-daemon semantics to be faithful to, so the
// extra ceremony (Calls(), Fail/FailOnce per-op) would not earn its keep
// here -- a handful of injectable, named error fields cover every scenario
// the acceptance criteria and the handlers' own branches need.
type fakeManager struct {
	mu        sync.Mutex
	agents    map[string]store.Agent
	nextID    int
	maxAgents int

	createErr error
	deleteErr error

	defaultBackend   string
	defaultModel     string
	defaultFastModel string
	defaultRepo      string

	anthropicKind      string
	anthropicValue     string
	anthropicUpdatedAt time.Time
	anthropicSet       bool
	anthropicGetErr    error
	anthropicSetErr    error
	anthropicClearErr  error

	loginActive   bool
	loginStartErr error
	loginStopErr  error
}

func newFakeManager(maxAgents int) *fakeManager {
	return &fakeManager{
		agents:           map[string]store.Agent{},
		maxAgents:        maxAgents,
		defaultBackend:   config.BackendOllama,
		defaultModel:     "glm-5.3:cloud",
		defaultFastModel: "glm-5.3-flash:cloud",
		defaultRepo:      "psenna/ai-sandbox.git",
	}
}

func (f *fakeManager) seed(a store.Agent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents[a.ID] = a
}

func (f *fakeManager) Create(_ context.Context, req agent.CreateRequest) (store.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return store.Agent{}, f.createErr
	}
	f.nextID++
	a := store.Agent{
		ID:          fmt.Sprintf("agt_%08d", f.nextID),
		Name:        req.Name,
		Description: req.Description,
		Backend:     req.Backend,
		Model:       req.Model,
		FastModel:   req.FastModel,
		Repo:        req.Repo,
		Status:      store.StatusCreating,
	}
	f.agents[a.ID] = a
	return a, nil
}

func (f *fakeManager) Delete(_ context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.agents, id) // matches agent.Manager.Delete: removing an absent agent is still success
	return nil
}

func (f *fakeManager) Get(_ context.Context, id string) (store.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[id]
	if !ok {
		return store.Agent{}, fmt.Errorf("getting agent %q: %w", id, store.ErrNotFound)
	}
	return a, nil
}

func (f *fakeManager) List(_ context.Context) ([]store.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Agent, 0, len(f.agents))
	for _, a := range f.agents {
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeManager) MaxAgents() int { return f.maxAgents }

func (f *fakeManager) DefaultBackend() string   { return f.defaultBackend }
func (f *fakeManager) DefaultModel() string     { return f.defaultModel }
func (f *fakeManager) DefaultFastModel() string { return f.defaultFastModel }
func (f *fakeManager) DefaultRepo() string      { return f.defaultRepo }

func (f *fakeManager) AnthropicAuthStatus(_ context.Context) (string, time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.anthropicGetErr != nil {
		return "", time.Time{}, false, f.anthropicGetErr
	}
	if !f.anthropicSet {
		return "", time.Time{}, false, nil
	}
	return f.anthropicKind, f.anthropicUpdatedAt, true, nil
}

func (f *fakeManager) SetAnthropicAuth(_ context.Context, kind, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.anthropicSetErr != nil {
		return f.anthropicSetErr
	}
	if !store.ValidAnthropicKind(kind) || value == "" {
		return fmt.Errorf("fakeManager: bad SetAnthropicAuth args kind=%q value-empty=%v", kind, value == "")
	}
	f.anthropicKind = kind
	f.anthropicValue = value
	f.anthropicUpdatedAt = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	f.anthropicSet = true
	return nil
}

func (f *fakeManager) ClearAnthropicAuth(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.anthropicClearErr != nil {
		return f.anthropicClearErr
	}
	f.anthropicSet = false
	f.anthropicKind = ""
	f.anthropicValue = ""
	f.anthropicUpdatedAt = time.Time{}
	return nil
}

func (f *fakeManager) StartAnthropicLogin(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loginStartErr != nil {
		return f.loginStartErr
	}
	f.loginActive = true
	return nil
}

func (f *fakeManager) StopAnthropicLogin(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loginStopErr != nil {
		return f.loginStopErr
	}
	f.loginActive = false
	return nil
}

func (f *fakeManager) AnthropicLoginActive(_ context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginActive, nil
}

func (f *fakeManager) Rename(_ context.Context, id string, name, description *string) (store.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[id]
	if !ok {
		return store.Agent{}, fmt.Errorf("renaming agent %q: %w", id, store.ErrNotFound)
	}
	if name != nil {
		a.Name = *name
	}
	if description != nil {
		a.Description = *description
	}
	f.agents[id] = a
	return a, nil
}

// --- test helpers -----------------------------------------------------------

func newTestHandler(mgr AgentManager, docker dockerclient.ExecClient) http.Handler {
	return NewHandler(mgr, docker, nil)
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshaling request body: %v", err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) ErrorEnvelope {
	t.Helper()
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding error envelope from %q: %v", rec.Body.String(), err)
	}
	return env
}

func decodeAgent(t *testing.T, rec *httptest.ResponseRecorder) store.Agent {
	t.Helper()
	var a store.Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("decoding agent from %q: %v", rec.Body.String(), err)
	}
	return a
}

// --- GET/POST /api/agents ---------------------------------------------------

func TestCreate(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{Name: "alpha", Description: "the first one"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
	}
	a := decodeAgent(t, rec)
	if a.Name != "alpha" || a.Description != "the first one" || a.ID == "" {
		t.Errorf("created agent = %+v, want a non-empty ID and the requested name/description", a)
	}
	if got := rec.Header().Get("Location"); got != "/api/agents/"+a.ID {
		t.Errorf("Location header = %q, want %q", got, "/api/agents/"+a.ID)
	}
}

func TestCreate_Repo(t *testing.T) {
	t.Run("a valid per-agent repo is passed through and recorded", func(t *testing.T) {
		mgr := newFakeManager(5)
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{Repo: "acme/widget.git"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
		}
		if a := decodeAgent(t, rec); a.Repo != "acme/widget.git" {
			t.Errorf("created agent Repo = %q, want the requested repo", a.Repo)
		}
	})

	t.Run("a malformed repo is a 400 before the manager is called", func(t *testing.T) {
		mgr := newFakeManager(5)
		mgr.createErr = errors.New("Create must not be reached")
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{Repo: "https://github.com/acme/widget"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
		}
		if got := decodeEnvelope(t, rec).Error.Code; got != CodeInvalidParam {
			t.Errorf("error code = %q, want %q", got, CodeInvalidParam)
		}
	})
}

func TestCreate_EmptyBodyIsValid(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
	}
}

func TestCreate_MalformedJSON(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	r := httptest.NewRequest("POST", "/api/agents", bytes.NewReader([]byte("{not json")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeBadJSON {
		t.Errorf("error code = %q, want %q", got, CodeBadJSON)
	}
}

func TestCreate_UnknownField(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	r := httptest.NewRequest("POST", "/api/agents", bytes.NewReader([]byte(`{"nam":"typo"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
	}
}

func TestCreate_AtCapacity(t *testing.T) {
	mgr := newFakeManager(1)
	mgr.createErr = fmt.Errorf("creating agent: %w", store.ErrAtCapacity)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{Name: "over-cap"})

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeAtCapacity {
		t.Errorf("error code = %q, want %q", got, CodeAtCapacity)
	}
}

func TestCreate_UnexpectedErrorIs500(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.createErr = errors.New("the docker daemon is on fire")
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{Name: "x"})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusInternalServerError, rec.Body)
	}
	if strBody := rec.Body.String(); bytes.Contains([]byte(strBody), []byte("fire")) {
		t.Errorf("500 body leaked the internal error text: %s", strBody)
	}
}

func TestList(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_a", Name: "a"})
	mgr.seed(store.Agent{ID: "agt_b", Name: "b"})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	var resp agentListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Errorf("Agents = %+v, want 2 entries", resp.Agents)
	}
	if resp.MaxAgents != 5 {
		t.Errorf("MaxAgents = %d, want 5", resp.MaxAgents)
	}
	if resp.DefaultBackend != "ollama" || resp.DefaultModel != "glm-5.3:cloud" || resp.DefaultFastModel != "glm-5.3-flash:cloud" {
		t.Errorf("defaults = %q/%q/%q, want the operator's configured create-form defaults",
			resp.DefaultBackend, resp.DefaultModel, resp.DefaultFastModel)
	}
	if resp.DefaultRepo != "psenna/ai-sandbox.git" {
		t.Errorf("DefaultRepo = %q, want the operator's configured repo", resp.DefaultRepo)
	}
}

func TestList_Empty(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"agents":[]`)) {
		t.Errorf("body = %s, want an explicit empty array, not null", rec.Body.String())
	}
}

// --- GET/PATCH/DELETE /api/agents/{id} --------------------------------------

func TestGet(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_a", Name: "a", Status: store.StatusRunning})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_a", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	if a := decodeAgent(t, rec); a.ID != "agt_a" || a.Status != store.StatusRunning {
		t.Errorf("agent = %+v, want the seeded record", a)
	}
}

func TestGet_NotFound(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_missing", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeNotFound {
		t.Errorf("error code = %q, want %q", got, CodeNotFound)
	}
}

func TestRename(t *testing.T) {
	cases := []struct {
		name            string
		req             patchAgentRequest
		wantName        string
		wantDescription string
	}{
		{"name only", patchAgentRequest{Name: ptr("renamed")}, "renamed", "original description"},
		{"description only", patchAgentRequest{Description: ptr("")}, "original name", ""},
		{"both", patchAgentRequest{Name: ptr("new"), Description: ptr("new desc")}, "new", "new desc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newFakeManager(5)
			mgr.seed(store.Agent{ID: "agt_a", Name: "original name", Description: "original description"})
			h := newTestHandler(mgr, dockerclienttest.New())

			rec := doJSON(t, h, "PATCH", "/api/agents/agt_a", tc.req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
			}
			a := decodeAgent(t, rec)
			if a.Name != tc.wantName || a.Description != tc.wantDescription {
				t.Errorf("agent = %+v, want name=%q description=%q", a, tc.wantName, tc.wantDescription)
			}
		})
	}
}

func TestRename_NeitherFieldProvidedIs400(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_a", Name: "a"})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PATCH", "/api/agents/agt_a", patchAgentRequest{})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeMissingField {
		t.Errorf("error code = %q, want %q", got, CodeMissingField)
	}
}

func TestRename_NotFound(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PATCH", "/api/agents/agt_missing", patchAgentRequest{Name: ptr("x")})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body)
	}
}

func TestDelete(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_a", Name: "a"})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "DELETE", "/api/agents/agt_a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}

	if _, err := mgr.Get(context.Background(), "agt_a"); !store.IsNotFound(err) {
		t.Errorf("agent still present after delete: err = %v", err)
	}
}

// TestDelete_Idempotent matches agent.Manager.Delete's own idempotency: a
// second delete of an already-gone agent is still success, not 404 -- the
// caller's desired end state (no such agent) already holds.
func TestDelete_Idempotent(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	first := doJSON(t, h, "DELETE", "/api/agents/agt_never_existed", nil)
	second := doJSON(t, h, "DELETE", "/api/agents/agt_never_existed", nil)

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("status = %d, %d, want %d both times", first.Code, second.Code, http.StatusOK)
	}
}

// --- GET /api/agents/{id}/output ---------------------------------------------

func TestOutput(t *testing.T) {
	docker := dockerclienttest.New()
	ctx := context.Background()
	cid, err := docker.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "agent-container", Image: "agent:dev"})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := docker.ContainerStart(ctx, cid); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	docker.ExecOutput["sh -c if [ ! -f '/workspace/.agent-output.log' ]; then exit 0; fi; exec cat '/workspace/.agent-output.log' 2>&1"] = []byte("hello world\r\n")

	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_a", ContainerID: cid})
	h := newTestHandler(mgr, docker)

	rec := doJSON(t, h, "GET", "/api/agents/agt_a/output", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	if rec.Body.String() != "hello world\r\n" {
		t.Errorf("body = %q, want the raw captured content", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain (not JSON: output bytes aren't guaranteed valid UTF-8)", ct)
	}
}

func TestOutput_TailParam(t *testing.T) {
	docker := dockerclienttest.New()
	ctx := context.Background()
	cid, err := docker.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "agent-container", Image: "agent:dev"})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := docker.ContainerStart(ctx, cid); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	docker.ExecOutput["sh -c if [ ! -f '/workspace/.agent-output.log' ]; then exit 0; fi; exec tail -n 3 '/workspace/.agent-output.log' 2>&1"] = []byte("last three lines\r\n")

	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_a", ContainerID: cid})
	h := newTestHandler(mgr, docker)

	rec := doJSON(t, h, "GET", "/api/agents/agt_a/output?tail=3", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	if rec.Body.String() != "last three lines\r\n" {
		t.Errorf("body = %q, want the tail-seeded content", rec.Body.String())
	}
}

func TestOutput_InvalidTailParam(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_a", ContainerID: "whatever"})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_a/output?tail=not-a-number", nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
	}
	env := decodeEnvelope(t, rec)
	if env.Error.Code != CodeInvalidParam || env.Error.Field != "tail" {
		t.Errorf("error = %+v, want code=%q field=%q", env.Error, CodeInvalidParam, "tail")
	}
}

func TestOutput_NoContainerYet(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_a", Status: store.StatusCreating}) // ContainerID still empty
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_a/output", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty (no container to look for a log file in yet)", rec.Body.String())
	}
}

func TestOutput_AgentNotFound(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_missing/output", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body)
	}
}

func TestOutput_ContainerGoneIs404(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_a", ContainerID: "does-not-exist-on-the-daemon"})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_a/output", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body)
	}
}

// --- routing edge cases -------------------------------------------------------

func TestMethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "PUT", "/api/agents", nil)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusMethodNotAllowed, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeMethodNotAllowed {
		t.Errorf("error code = %q, want %q", got, CodeMethodNotAllowed)
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/nope", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body)
	}
}

func ptr(s string) *string { return &s }
