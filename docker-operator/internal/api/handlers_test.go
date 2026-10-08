package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/filestore"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
	"github.com/psenna/ai-sandbox/docker-operator/internal/wsbridge"
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

	updateErr  error
	updateReqs []agent.UpdateRequest

	purgedIDs []string
	purgeErr  error

	defaultBackend              string
	defaultModel                string
	defaultFastModel            string
	defaultOllamaURL            string
	defaultRepo                 string
	defaultAutoCompactThreshold string
	defaultMaxContextTokens     string
	defaultAutoMode             string

	dockerRuntime string

	anthropicAccounts      []store.AnthropicAccount
	anthropicDefaultID     string
	anthropicListErr       error
	anthropicCreateErr     error
	anthropicDeleteErr     error
	anthropicSetDefaultErr error

	loginActive   bool
	loginStartErr error
	loginStopErr  error

	imageInv     map[string]agent.ImageInventory
	imageInvErr  map[string]error
	refreshErr   map[string]error
	refreshCalls map[string]int
	defaultRefs  map[string]string

	// deleteTagCalls is keyed harness+"|"+tag so a test can assert both what
	// was asked for and how often. A successful delete ALSO removes the tag
	// from imageInv, so the read-back response the handler returns is
	// assertable.
	deleteTagCalls map[string]int
	deleteTagErr   map[string]error
	cleanupCalls   map[string]int
	cleanupErr     map[string]error
	cleanupReports map[string]agent.AgentImageCleanupReport
	pullCalls      map[string]int
	pullReports    map[string]agent.AgentImagePullReport

	templates map[string]store.Template

	nextTemplateID int

	listTemplatesErr error

	createTemplateErr error

	updateTemplateErr error

	deleteTemplateErr error
}

func newFakeManager(maxAgents int) *fakeManager {
	return &fakeManager{
		agents:           map[string]store.Agent{},
		templates:        map[string]store.Template{},
		maxAgents:        maxAgents,
		defaultBackend:   config.BackendOllama,
		defaultModel:     "glm-5.3:cloud",
		defaultFastModel: "glm-5.3-flash:cloud",
		defaultOllamaURL: "http://ollama:11434",
		defaultRepo:      "psenna/ai-sandbox.git",
		defaultAutoMode:  config.AutoModeOn,
		imageInv:         map[string]agent.ImageInventory{},
		imageInvErr:      map[string]error{},
		refreshErr:       map[string]error{},
		refreshCalls:     map[string]int{},
		defaultRefs:      map[string]string{},

		deleteTagCalls: map[string]int{},
		deleteTagErr:   map[string]error{},
		cleanupCalls:   map[string]int{},
		cleanupErr:     map[string]error{},
		cleanupReports: map[string]agent.AgentImageCleanupReport{},
		pullCalls:      map[string]int{},
		pullReports:    map[string]agent.AgentImagePullReport{},
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
		ID:                   fmt.Sprintf("agt_%08d", f.nextID),
		Name:                 req.Name,
		Description:          req.Description,
		Backend:              req.Backend,
		Harness:              req.Harness,
		Model:                req.Model,
		FastModel:            req.FastModel,
		OllamaURL:            req.OllamaURL,
		Repo:                 req.Repo,
		AutoCompactThreshold: req.AutoCompactThreshold,
		MaxContextTokens:     req.MaxContextTokens,
		AutoMode:             req.AutoMode,
		Status:               store.StatusCreating,
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

func (f *fakeManager) PurgeAgentFiles(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.purgeErr != nil {
		return f.purgeErr
	}
	f.purgedIDs = append(f.purgedIDs, id)
	return nil
}

func (f *fakeManager) Update(_ context.Context, id string, req agent.UpdateRequest) (store.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateReqs = append(f.updateReqs, req)
	if f.updateErr != nil {
		return store.Agent{}, f.updateErr
	}
	a, ok := f.agents[id]
	if !ok {
		return store.Agent{}, fmt.Errorf("updating agent %q: %w", id, store.ErrNotFound)
	}
	a.Name = req.Name
	a.Description = req.Description
	a.Backend = req.Backend
	a.Model = req.Model
	a.FastModel = req.FastModel
	a.OllamaURL = req.OllamaURL
	a.Repo = req.Repo
	a.AutoCompactThreshold = req.AutoCompactThreshold
	a.MaxContextTokens = req.MaxContextTokens
	a.AutoMode = req.AutoMode
	a.Status = store.StatusRunning
	f.agents[id] = a
	return a, nil
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

func (f *fakeManager) DefaultBackend() string              { return f.defaultBackend }
func (f *fakeManager) DefaultModel() string                { return f.defaultModel }
func (f *fakeManager) DefaultFastModel() string            { return f.defaultFastModel }
func (f *fakeManager) DefaultOllamaURL() string            { return f.defaultOllamaURL }
func (f *fakeManager) DefaultRepo() string                 { return f.defaultRepo }
func (f *fakeManager) DefaultAutoCompactThreshold() string { return f.defaultAutoCompactThreshold }
func (f *fakeManager) DefaultMaxContextTokens() string     { return f.defaultMaxContextTokens }
func (f *fakeManager) DefaultAutoMode() string             { return f.defaultAutoMode }

func (f *fakeManager) DockerRuntime() string { return f.dockerRuntime }

// fakeAgentRepo mirrors config.AgentImageFor's two real defaults.
func fakeAgentRepo(harness string) string {
	if config.NormalizeHarness(harness) == config.HarnessOpenCode {
		return "ghcr.io/psenna/ai-sandbox-agent-opencode"
	}
	return "ghcr.io/psenna/ai-sandbox-agent"
}

// fakeInventory builds one harness's inventory from published+local tag
// lists, computing DefaultTag/Options the way the real Manager would.
func fakeInventory(harness string, registryTags, localTags []string) agent.ImageInventory {
	h := config.NormalizeHarness(harness)
	def := agent.NewestDateTimeTag(localTags)
	if def == "" {
		def = agent.NewestDateTimeTag(registryTags)
	}
	if def == "" {
		def = "latest"
	}
	return agent.ImageInventory{
		Harness:      h,
		Repo:         fakeAgentRepo(h),
		RegistryTags: registryTags,
		LocalTags:    localTags,
		DefaultTag:   def,
		Options:      agent.OfferedImageTags(registryTags, localTags, def),
	}
}

func (f *fakeManager) setInventory(harness string, inv agent.ImageInventory) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imageInv[config.NormalizeHarness(harness)] = inv
}

func (f *fakeManager) AgentImageInventory(_ context.Context, harness string) (agent.ImageInventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := config.NormalizeHarness(harness)
	if err := f.imageInvErr[h]; err != nil {
		return agent.ImageInventory{}, err
	}
	if inv, ok := f.imageInv[h]; ok {
		return inv, nil
	}
	return agent.ImageInventory{Harness: h, Repo: fakeAgentRepo(h)}, nil
}

func (f *fakeManager) DefaultAgentImageRef(_ context.Context, harness string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := config.NormalizeHarness(harness)
	if ref, ok := f.defaultRefs[h]; ok {
		return ref
	}
	if inv, ok := f.imageInv[h]; ok && inv.DefaultTag != "" {
		return inv.Repo + ":" + inv.DefaultTag
	}
	return fakeAgentRepo(h) + ":latest"
}

func (f *fakeManager) RefreshAgentImageTags(_ context.Context, harness string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := config.NormalizeHarness(harness)
	f.refreshCalls[h]++
	return f.refreshErr[h]
}

func (f *fakeManager) DeleteAgentImageTag(_ context.Context, harness, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := config.NormalizeHarness(harness)
	f.deleteTagCalls[h+"|"+tag]++
	if err := f.deleteTagErr[h+"|"+tag]; err != nil {
		return err
	}
	// A successful delete really removes the tag from the inventory, so the
	// read-back response the handler returns after it is assertable -- the
	// whole point of returning the refreshed map from the action.
	if inv, ok := f.imageInv[h]; ok {
		opts := make([]agent.ImageTagOption, 0, len(inv.Options))
		for _, o := range inv.Options {
			if o.Tag != tag {
				opts = append(opts, o)
			}
		}
		local := make([]string, 0, len(inv.LocalTags))
		for _, t := range inv.LocalTags {
			if t != tag {
				local = append(local, t)
			}
		}
		inv.Options, inv.LocalTags = opts, local
		if inv.DefaultTag == tag {
			inv.DefaultTag = ""
		}
		f.imageInv[h] = inv
	}
	return nil
}

func (f *fakeManager) CleanupAgentImages(_ context.Context, harness string) (agent.AgentImageCleanupReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := config.NormalizeHarness(harness)
	f.cleanupCalls[h]++
	if rep, ok := f.cleanupReports[h]; ok {
		return rep, nil
	}
	if err := f.cleanupErr[h]; err != nil {
		return agent.AgentImageCleanupReport{Harness: h}, err
	}
	return agent.AgentImageCleanupReport{Harness: h}, nil
}

func (f *fakeManager) PullLatestAgentImage(_ context.Context, harness string) agent.AgentImagePullReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := config.NormalizeHarness(harness)
	f.pullCalls[h]++
	if rep, ok := f.pullReports[h]; ok {
		return rep
	}
	return agent.AgentImagePullReport{Harness: h}
}

func (f *fakeManager) ListAnthropicAccounts(_ context.Context) ([]store.AnthropicAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.anthropicListErr != nil {
		return nil, f.anthropicListErr
	}
	out := make([]store.AnthropicAccount, len(f.anthropicAccounts))
	copy(out, f.anthropicAccounts)
	return out, nil
}

func (f *fakeManager) CreateAnthropicAccount(_ context.Context, name, kind, value string) (store.AnthropicAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.anthropicCreateErr != nil {
		return store.AnthropicAccount{}, f.anthropicCreateErr
	}
	for _, a := range f.anthropicAccounts {
		if a.Name == name {
			return store.AnthropicAccount{}, fmt.Errorf("creating anthropic account %q: %w", name, store.ErrAnthropicAccountNameTaken)
		}
	}
	a := store.AnthropicAccount{
		ID:   fmt.Sprintf("anc_fake%d", len(f.anthropicAccounts)+1),
		Name: name, Kind: kind, Value: value,
		UpdatedAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
	}
	f.anthropicAccounts = append(f.anthropicAccounts, a)
	if f.anthropicDefaultID == "" {
		f.anthropicDefaultID = a.ID
	}
	return a, nil
}

func (f *fakeManager) DeleteAnthropicAccount(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.anthropicDeleteErr != nil {
		return f.anthropicDeleteErr
	}
	kept := f.anthropicAccounts[:0]
	for _, a := range f.anthropicAccounts {
		if a.ID != id {
			kept = append(kept, a)
		}
	}
	f.anthropicAccounts = kept
	if f.anthropicDefaultID == id {
		f.anthropicDefaultID = ""
	}
	return nil
}

func (f *fakeManager) DefaultAnthropicAccountID(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.anthropicDefaultID, nil
}

func (f *fakeManager) SetDefaultAnthropicAccount(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.anthropicSetDefaultErr != nil {
		return f.anthropicSetDefaultErr
	}
	for _, a := range f.anthropicAccounts {
		if a.ID == id {
			f.anthropicDefaultID = id
			return nil
		}
	}
	return fmt.Errorf("setting default anthropic account %q: %w", id, store.ErrAnthropicAccountNotFound)
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

func (f *fakeManager) Rename(_ context.Context, id string, name, description, accountID *string) (store.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[id]
	if !ok {
		return store.Agent{}, fmt.Errorf("renaming agent %q: %w", id, store.ErrNotFound)
	}
	if accountID != nil {
		if a.Backend != config.BackendAnthropic {
			return store.Agent{}, fmt.Errorf("renaming agent %q: %w", id, agent.ErrAgentNotAnthropic)
		}
		found := false
		for _, acc := range f.anthropicAccounts {
			if acc.ID == *accountID {
				found = true
				break
			}
		}
		if !found {
			return store.Agent{}, fmt.Errorf("renaming agent %q: %w: %q", id, agent.ErrUnknownAnthropicAccount, *accountID)
		}
		a.AnthropicAccountID = *accountID
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

func (f *fakeManager) findTemplateByName(excludeID, name string) bool {
	for id, t := range f.templates {
		if id != excludeID && t.Name == name {
			return true
		}
	}
	return false
}

func (f *fakeManager) ListTemplates(_ context.Context) ([]store.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listTemplatesErr != nil {
		return nil, f.listTemplatesErr
	}
	out := make([]store.Template, 0, len(f.templates))
	for _, t := range f.templates {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeManager) CreateTemplate(_ context.Context, spec store.TemplateCreateSpec) (store.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createTemplateErr != nil {
		return store.Template{}, f.createTemplateErr
	}
	if f.findTemplateByName("", spec.Name) {
		return store.Template{}, fmt.Errorf("creating template: %w", store.ErrTemplateNameExists)
	}
	f.nextTemplateID++
	t := store.Template{
		ID:                   fmt.Sprintf("tpl_%08d", f.nextTemplateID),
		Name:                 spec.Name,
		Description:          spec.Description,
		Backend:              spec.Backend,
		Model:                spec.Model,
		FastModel:            spec.FastModel,
		OllamaURL:            spec.OllamaURL,
		Repo:                 spec.Repo,
		AutoCompactThreshold: spec.AutoCompactThreshold,
		MaxContextTokens:     spec.MaxContextTokens,
		ImageTag:             spec.ImageTag,
		AutoMode:             spec.AutoMode,
	}
	f.templates[t.ID] = t
	return t, nil
}

func (f *fakeManager) UpdateTemplate(_ context.Context, id string, spec store.TemplateCreateSpec) (store.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateTemplateErr != nil {
		return store.Template{}, f.updateTemplateErr
	}
	t, ok := f.templates[id]
	if !ok {
		return store.Template{}, fmt.Errorf("updating template %q: %w", id, store.ErrTemplateNotFound)
	}
	if f.findTemplateByName(id, spec.Name) {
		return store.Template{}, fmt.Errorf("updating template %q: %w", id, store.ErrTemplateNameExists)
	}
	t.Name = spec.Name
	t.Description = spec.Description
	t.Backend = spec.Backend
	t.Model = spec.Model
	t.FastModel = spec.FastModel
	t.OllamaURL = spec.OllamaURL
	t.Repo = spec.Repo
	t.AutoCompactThreshold = spec.AutoCompactThreshold
	t.MaxContextTokens = spec.MaxContextTokens
	t.ImageTag = spec.ImageTag
	t.AutoMode = spec.AutoMode
	f.templates[id] = t
	return t, nil
}

func (f *fakeManager) DeleteTemplate(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteTemplateErr != nil {
		return f.deleteTemplateErr
	}
	delete(f.templates, id)
	return nil
}

// --- test helpers -----------------------------------------------------------

func newTestHandler(mgr AgentManager, docker execStatsClient) http.Handler {
	return NewHandler(mgr, docker, nil, 0, nil)
}

func newTestHandlerFiles(mgr AgentManager, docker execStatsClient, fs *filestore.Store, max int64) http.Handler {
	return NewHandler(mgr, docker, fs, max, nil)
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

func TestCreate_OllamaURL(t *testing.T) {
	t.Run("a valid per-agent ollama_url is passed through and recorded", func(t *testing.T) {
		mgr := newFakeManager(5)
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{Backend: "ollama", OllamaURL: "http://gpu-box:11434"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
		}
		if a := decodeAgent(t, rec); a.OllamaURL != "http://gpu-box:11434" {
			t.Errorf("created agent OllamaURL = %q, want the requested override", a.OllamaURL)
		}
	})

	t.Run("a malformed ollama_url is a 400 before the manager is called", func(t *testing.T) {
		mgr := newFakeManager(5)
		mgr.createErr = errors.New("Create must not be reached")
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{OllamaURL: "gpu-box:11434"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
		}
		env := decodeEnvelope(t, rec)
		if env.Error.Code != CodeInvalidParam || env.Error.Field != "ollama_url" {
			t.Errorf("error = %+v, want code %q field %q", env.Error, CodeInvalidParam, "ollama_url")
		}
	})

	t.Run("ollama_url with the anthropic backend is a 400", func(t *testing.T) {
		mgr := newFakeManager(5)
		mgr.createErr = errors.New("Create must not be reached")
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{Backend: "anthropic", OllamaURL: "http://gpu-box:11434"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
		}
		if got := decodeEnvelope(t, rec).Error.Code; got != CodeInvalidParam {
			t.Errorf("error code = %q, want %q", got, CodeInvalidParam)
		}
	})
}

func TestCreate_AutoCompactThreshold(t *testing.T) {
	t.Run("a valid per-agent auto_compact_threshold is passed through and recorded", func(t *testing.T) {
		mgr := newFakeManager(5)
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{AutoCompactThreshold: "85"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
		}
		if a := decodeAgent(t, rec); a.AutoCompactThreshold != "85" {
			t.Errorf("created agent AutoCompactThreshold = %q, want the requested value", a.AutoCompactThreshold)
		}
	})

	for _, bad := range []string{"49", "101", "abc", "85.5", "-20"} {
		t.Run("an out-of-range auto_compact_threshold "+bad+" is a 400 before the manager is called", func(t *testing.T) {
			mgr := newFakeManager(5)
			mgr.createErr = errors.New("Create must not be reached")
			h := newTestHandler(mgr, dockerclienttest.New())

			rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{AutoCompactThreshold: bad})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
			}
			env := decodeEnvelope(t, rec)
			if env.Error.Code != CodeInvalidParam || env.Error.Field != "auto_compact_threshold" {
				t.Errorf("error = %+v, want code %q field %q", env.Error, CodeInvalidParam, "auto_compact_threshold")
			}
		})
	}
}

func TestCreate_AutoMode(t *testing.T) {
	for _, v := range []string{"", "on", "off"} {
		t.Run("auto_mode="+v+" is passed through and recorded", func(t *testing.T) {
			mgr := newFakeManager(5)
			h := newTestHandler(mgr, dockerclienttest.New())

			rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{AutoMode: v})
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
			}
			if a := decodeAgent(t, rec); a.AutoMode != v {
				t.Errorf("created agent AutoMode = %q, want %q", a.AutoMode, v)
			}
		})
	}

	for _, bad := range []string{"On", "true", "1", "always"} {
		t.Run("an invalid auto_mode "+bad+" is a 400 before the manager is called", func(t *testing.T) {
			mgr := newFakeManager(5)
			mgr.createErr = errors.New("Create must not be reached")
			h := newTestHandler(mgr, dockerclienttest.New())

			rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{AutoMode: bad})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
			}
			env := decodeEnvelope(t, rec)
			if env.Error.Code != CodeInvalidParam || env.Error.Field != "auto_mode" {
				t.Errorf("error = %+v, want code %q field %q", env.Error, CodeInvalidParam, "auto_mode")
			}
		})
	}
}

func TestCreate_MaxContextTokens(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{MaxContextTokens: "200000"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
	}
	if a := decodeAgent(t, rec); a.MaxContextTokens != "200000" {
		t.Errorf("created agent MaxContextTokens = %q, want the requested value", a.MaxContextTokens)
	}
}

func TestCreate_Harness(t *testing.T) {
	t.Run("harness=opencode with backend=ollama is created", func(t *testing.T) {
		mgr := newFakeManager(5)
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{Harness: "opencode", Backend: "ollama"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
		}
		if a := decodeAgent(t, rec); a.Harness != "opencode" {
			t.Errorf("created agent Harness = %q, want %q", a.Harness, "opencode")
		}
	})

	t.Run("harness=opencode with backend=anthropic is a 400 before the manager is called", func(t *testing.T) {
		mgr := newFakeManager(5)
		mgr.createErr = errors.New("Create must not be reached")
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{Harness: "opencode", Backend: "anthropic"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
		}
		env := decodeEnvelope(t, rec)
		if env.Error.Code != CodeInvalidParam || env.Error.Field != "harness" {
			t.Errorf("error = %+v, want code %q field %q", env.Error, CodeInvalidParam, "harness")
		}
	})

	t.Run("an omitted harness is passed through unchanged", func(t *testing.T) {
		mgr := newFakeManager(5)
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
		}
		if a := decodeAgent(t, rec); a.Harness != "" {
			t.Errorf("created agent Harness = %q, want empty (unchanged)", a.Harness)
		}
	})

	t.Run("harness=claude-code with backend=anthropic is allowed", func(t *testing.T) {
		mgr := newFakeManager(5)
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{Harness: "claude-code", Backend: "anthropic"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
		}
		if a := decodeAgent(t, rec); a.Harness != "claude-code" {
			t.Errorf("created agent Harness = %q, want %q", a.Harness, "claude-code")
		}
	})

	for _, bad := range []string{"claude", "Claude-Code", "open-code", "opencode "} {
		t.Run("an invalid harness "+bad+" is a 400 before the manager is called", func(t *testing.T) {
			mgr := newFakeManager(5)
			mgr.createErr = errors.New("Create must not be reached")
			h := newTestHandler(mgr, dockerclienttest.New())

			rec := doJSON(t, h, "POST", "/api/agents", createAgentRequest{Harness: bad})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
			}
			env := decodeEnvelope(t, rec)
			if env.Error.Code != CodeInvalidParam || env.Error.Field != "harness" {
				t.Errorf("error = %+v, want code %q field %q", env.Error, CodeInvalidParam, "harness")
			}
		})
	}
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
	if resp.DefaultOllamaURL != "http://ollama:11434" {
		t.Errorf("DefaultOllamaURL = %q, want the operator's configured Ollama URL", resp.DefaultOllamaURL)
	}
	if resp.DefaultAutoCompactThreshold != "" || resp.DefaultMaxContextTokens != "" {
		t.Errorf("DefaultAutoCompactThreshold/DefaultMaxContextTokens = %q/%q, want empty when the operator set neither default",
			resp.DefaultAutoCompactThreshold, resp.DefaultMaxContextTokens)
	}
	if resp.DefaultAutoMode != "on" {
		t.Errorf("DefaultAutoMode = %q, want %q (newFakeManager's default)", resp.DefaultAutoMode, "on")
	}

	mgr.defaultAutoCompactThreshold = "85"
	mgr.defaultMaxContextTokens = "200000"
	mgr.defaultAutoMode = "off"
	rec = doJSON(t, h, "GET", "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	resp = agentListResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	if resp.DefaultAutoCompactThreshold != "85" || resp.DefaultMaxContextTokens != "200000" {
		t.Errorf("defaults = %q/%q, want the operator's configured thresholds",
			resp.DefaultAutoCompactThreshold, resp.DefaultMaxContextTokens)
	}
	if resp.DefaultAutoMode != "off" {
		t.Errorf("DefaultAutoMode = %q, want %q", resp.DefaultAutoMode, "off")
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

func TestHandleList_UpgradeAvailableComputed(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.setInventory(config.HarnessClaudeCode, fakeInventory(config.HarnessClaudeCode,
		[]string{"20260901-120000", "20260801-120000"}, nil))
	mgr.seed(store.Agent{ID: "agt_old", Name: "old", Image: "ghcr.io/psenna/ai-sandbox-agent:20260801-120000"})
	mgr.seed(store.Agent{ID: "agt_new", Name: "new", Image: "ghcr.io/psenna/ai-sandbox-agent:20260901-120000"})
	mgr.seed(store.Agent{ID: "agt_latest", Name: "latest", Image: "ghcr.io/psenna/ai-sandbox-agent:latest"})
	mgr.seed(store.Agent{ID: "agt_none", Name: "none", Image: ""})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}

	var resp struct {
		Agents []struct {
			ID               string `json:"id"`
			Image            string `json:"image"`
			UpgradeAvailable bool   `json:"upgrade_available"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	want := map[string]bool{"agt_old": true, "agt_new": false, "agt_latest": false, "agt_none": false}
	got := map[string]bool{}
	for _, a := range resp.Agents {
		got[a.ID] = a.UpgradeAvailable
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("agent %s upgrade_available = %v, want %v (body: %s)", id, got[id], w, rec.Body)
		}
	}
	// The embedded store.Agent fields stay at the top level.
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"image":"ghcr.io/psenna/ai-sandbox-agent:20260801-120000"`)) {
		t.Errorf("list body missing the flattened image field: %s", rec.Body)
	}
}

func TestHandleList_NoTagsNoUpgrades(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_a", Name: "a", Image: "ghcr.io/psenna/ai-sandbox-agent:20260801-120000"})
	mgr.seed(store.Agent{ID: "agt_b", Name: "b", Image: "ghcr.io/psenna/ai-sandbox-agent:20260101-000000"})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	var resp struct {
		Agents []struct {
			UpgradeAvailable bool `json:"upgrade_available"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(resp.Agents))
	}
	for i, a := range resp.Agents {
		if a.UpgradeAvailable {
			t.Errorf("agent[%d] upgrade_available = true, want false with no discovered tags", i)
		}
	}
}

// TestHandleList_UpgradeIsPerHarness is the regression test for issue #199:
// buildAgentViews used to compare EVERY agent against claude-code's tag list
// regardless of its own harness, so an opencode agent already on the newest
// opencode tag was flagged as needing an upgrade just because it looked old
// against claude-code's (unrelated) tag list.
func TestHandleList_UpgradeIsPerHarness(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.setInventory(config.HarnessClaudeCode, fakeInventory(config.HarnessClaudeCode,
		[]string{"20260910-120000", "20260901-120000"}, nil))
	mgr.setInventory(config.HarnessOpenCode, fakeInventory(config.HarnessOpenCode,
		[]string{"20260905-120000", "20260801-120000"}, nil))

	mgr.seed(store.Agent{ID: "agt_oc_current", Harness: config.HarnessOpenCode,
		Image: "ghcr.io/psenna/ai-sandbox-agent-opencode:20260906-120000"})
	mgr.seed(store.Agent{ID: "agt_cc_old", Harness: config.HarnessClaudeCode,
		Image: "ghcr.io/psenna/ai-sandbox-agent:20260906-120000"})
	mgr.seed(store.Agent{ID: "agt_oc_old", Harness: config.HarnessOpenCode,
		Image: "ghcr.io/psenna/ai-sandbox-agent-opencode:20260701-120000"})
	mgr.seed(store.Agent{ID: "agt_legacy",
		Image: "ghcr.io/psenna/ai-sandbox-agent:20260906-120000"})

	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "GET", "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Agents []struct {
			ID               string `json:"id"`
			UpgradeAvailable bool   `json:"upgrade_available"`
			UpgradeReady     bool   `json:"upgrade_ready"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	wantAvailable := map[string]bool{
		"agt_oc_current": false, // the reported bug: reads true before the fix
		"agt_cc_old":     true,
		"agt_oc_old":     true,
		"agt_legacy":     true,
	}
	got := map[string]bool{}
	gotReady := map[string]bool{}
	for _, a := range resp.Agents {
		got[a.ID] = a.UpgradeAvailable
		gotReady[a.ID] = a.UpgradeReady
	}
	for id, w := range wantAvailable {
		if got[id] != w {
			t.Errorf("agent %s upgrade_available = %v, want %v (each agent must be compared against its OWN harness's tags); body: %s",
				id, got[id], w, rec.Body)
		}
		if gotReady[id] {
			t.Errorf("agent %s upgrade_ready = true, want false: no tag is present on this host", id)
		}
	}
}

// TestHandleList_UpgradeReadyOnlyWhenLocallyPresent proves UpgradeReady is
// computed against the HOST's local tags (agent.ImageInventory.LocalTags),
// not the registry snapshot: a newer tag published but not yet pulled must
// report upgrade_available without upgrade_ready.
func TestHandleList_UpgradeReadyOnlyWhenLocallyPresent(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.setInventory(config.HarnessClaudeCode, fakeInventory(config.HarnessClaudeCode,
		[]string{"20260910-120000", "20260901-120000"},
		[]string{"20260910-120000", "20260901-120000"}))
	mgr.setInventory(config.HarnessOpenCode, fakeInventory(config.HarnessOpenCode,
		[]string{"20260910-120000", "20260901-120000"},
		[]string{"20260901-120000"}))

	mgr.seed(store.Agent{ID: "agt_cc", Harness: config.HarnessClaudeCode,
		Image: "ghcr.io/psenna/ai-sandbox-agent:20260901-120000"})
	mgr.seed(store.Agent{ID: "agt_oc", Harness: config.HarnessOpenCode,
		Image: "ghcr.io/psenna/ai-sandbox-agent-opencode:20260901-120000"})

	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "GET", "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Agents []struct {
			ID               string `json:"id"`
			UpgradeAvailable bool   `json:"upgrade_available"`
			UpgradeReady     bool   `json:"upgrade_ready"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	wantAvailable := map[string]bool{"agt_cc": true, "agt_oc": true}
	wantReady := map[string]bool{"agt_cc": true, "agt_oc": false}
	got := map[string]bool{}
	gotReady := map[string]bool{}
	for _, a := range resp.Agents {
		got[a.ID] = a.UpgradeAvailable
		gotReady[a.ID] = a.UpgradeReady
	}
	for id, w := range wantAvailable {
		if got[id] != w {
			t.Errorf("agent %s upgrade_available = %v, want %v; body: %s", id, got[id], w, rec.Body)
		}
	}
	for id, w := range wantReady {
		if gotReady[id] != w {
			t.Errorf("agent %s upgrade_ready = %v, want %v (agt_cc's newer tag is locally present, agt_oc's is not); body: %s",
				id, gotReady[id], w, rec.Body)
		}
	}
}

func TestHandleGet_IncludesImageAndUpgradeAvailable(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.setInventory(config.HarnessClaudeCode, fakeInventory(config.HarnessClaudeCode,
		[]string{"20260901-120000", "20260801-120000"}, nil))
	mgr.seed(store.Agent{ID: "agt_a", Name: "a", Image: "ghcr.io/psenna/ai-sandbox-agent:20260801-120000"})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	var resp struct {
		Image            string `json:"image"`
		UpgradeAvailable bool   `json:"upgrade_available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding agent: %v", err)
	}
	if resp.Image != "ghcr.io/psenna/ai-sandbox-agent:20260801-120000" {
		t.Errorf("image = %q, want the seeded image", resp.Image)
	}
	if !resp.UpgradeAvailable {
		t.Errorf("upgrade_available = false, want true (a newer date-time tag exists)")
	}
}

func TestAgentView_JSONShapeIsFlat(t *testing.T) {
	at := time.Unix(1700000000, 0)
	b, err := json.Marshal(agentView{
		Agent:            store.Agent{ID: "agt_a", Name: "a", Status: store.StatusRunning, Image: "ghcr.io/x/y:latest"},
		UpgradeAvailable: true,
		UpgradeReady:     true,
		Activity:         "working",
		ActivityAt:       &at,
		Resources:        &agentResources{CPUPercent: 12.5, MemUsedBytes: 340 << 20, MemLimitBytes: 2 << 30, Pids: 3},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"id", "name", "status", "image", "upgrade_available", "upgrade_ready", "activity", "activity_at", "resources"} {
		if _, ok := m[k]; !ok {
			t.Errorf("marshalled agentView missing top-level key %q; got %s", k, b)
		}
	}

	// The omission direction: with no activity signal, activity_at must be
	// absent entirely -- not a bare time.Time's zero value ("0001-01-01...").
	// The *time.Time is what makes that possible. Same shape for resources: a
	// bare agentView must not marshal it (the pointer is what lets omitempty
	// drop the whole object, so a stopped/error agent reports NOTHING rather
	// than a zeroed one).
	bare, err := json.Marshal(agentView{Agent: store.Agent{ID: "agt_b", Name: "b"}})
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
	var bareMap map[string]json.RawMessage
	if err := json.Unmarshal(bare, &bareMap); err != nil {
		t.Fatalf("unmarshal bare: %v", err)
	}
	if _, ok := bareMap["activity_at"]; ok {
		t.Errorf("bare agentView marshalled activity_at, want it omitted: %s", bare)
	}
	if _, ok := bareMap["resources"]; ok {
		t.Errorf("bare agentView marshalled resources, want it omitted: %s", bare)
	}
}

// TestHandleList_ActivityAt is the regression test for issue #217's one
// backend addition: buildAgentViews used to discard ReadActivity's timestamp,
// so the Activity page could only say "Working" with no idea how long for.
// It proves the timestamp reaches the wire, that a stopped agent gets neither
// field, and that only running agents exec at all.
//
// The exec key is rebuilt from the exported wsbridge.ActivityLogPath literal
// (exactly as wsbridge's own activity_test.go builds it) rather than from the
// unexported activityReadCmd -- if that command's shape ever drifts, this test
// fails loudly on the missing activity_at, which is the point.
func TestHandleList_ActivityAt(t *testing.T) {
	fake := dockerclienttest.New()
	ctx := context.Background()
	containerID, err := fake.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "agent", Image: "agent:dev"})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := fake.ContainerStart(ctx, containerID); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	key := strings.Join([]string{"sh", "-c",
		"if [ ! -f '" + wsbridge.ActivityLogPath + "' ]; then exit 0; fi; exec cat '" + wsbridge.ActivityLogPath + "' 2>&1"}, " ")
	fake.ExecOutput[key] = []byte("working 1700000000\n")
	fake.ExecExit[key] = 0

	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_run", Name: "run", Status: store.StatusRunning, ContainerID: containerID})
	mgr.seed(store.Agent{ID: "agt_stop", Name: "stop", Status: store.StatusStopped, ContainerID: containerID})
	h := newTestHandler(mgr, fake)

	rec := doJSON(t, h, "GET", "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Agents []struct {
			ID         string     `json:"id"`
			Activity   string     `json:"activity"`
			ActivityAt *time.Time `json:"activity_at"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Fatalf("agents = %d, want 2; body: %s", len(resp.Agents), rec.Body)
	}
	want := time.Unix(1700000000, 0)
	for _, a := range resp.Agents {
		switch a.ID {
		case "agt_run":
			if a.Activity != "working" {
				t.Errorf("running agent activity = %q, want %q", a.Activity, "working")
			}
			if a.ActivityAt == nil || !a.ActivityAt.Equal(want) {
				t.Errorf("running agent activity_at = %v, want %v; body: %s", a.ActivityAt, want, rec.Body)
			}
		case "agt_stop":
			if a.Activity != "" || a.ActivityAt != nil {
				t.Errorf("stopped agent = (%q, %v), want no activity signal", a.Activity, a.ActivityAt)
			}
		default:
			t.Errorf("unexpected agent id %q", a.ID)
		}
	}

	// Only the running agent may exec; the stopped one must not be read.
	execs := 0
	for _, s := range fake.ExecSpecs() {
		if strings.Join(s.Cmd, " ") == key {
			execs++
		}
	}
	if execs != 1 {
		t.Errorf("activity exec ran %d times, want 1 (only the running agent)", execs)
	}
}

// TestHandleList_Resources is the #218 regression test: a running agent's
// resources object reaches the wire aggregated over BOTH its containers (agent
// + DinD sidecar) -- cpu and memory SUMMED, the memory limit the MAX (each
// container reports the host total, so summing would double it), pids summed
// -- while a stopped agent reports no resources key at all, and only the
// running agent's containers are ever read.
func TestHandleList_Resources(t *testing.T) {
	fake := dockerclienttest.New()
	ctx := context.Background()

	id1, err := fake.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "agent", Image: "agent:dev"})
	if err != nil {
		t.Fatalf("ContainerCreate(agent): %v", err)
	}
	id2, err := fake.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "dind", Image: "docker:27-dind"})
	if err != nil {
		t.Fatalf("ContainerCreate(dind): %v", err)
	}
	for _, id := range []string{id1, id2} {
		if err := fake.ContainerStart(ctx, id); err != nil {
			t.Fatalf("ContainerStart(%s): %v", id, err)
		}
	}
	if err := fake.SetStats(id1, dockerclient.Stats{CPUPercent: 10.0, MemoryUsed: 100 << 20, MemoryLimit: 1 << 30, Pids: 2}); err != nil {
		t.Fatalf("SetStats(agent): %v", err)
	}
	if err := fake.SetStats(id2, dockerclient.Stats{CPUPercent: 32.5, MemoryUsed: 240 << 20, MemoryLimit: 2 << 30, Pids: 5}); err != nil {
		t.Fatalf("SetStats(dind): %v", err)
	}

	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_run", Name: "run", Status: store.StatusRunning, ContainerID: id1, DindContainerID: id2})
	mgr.seed(store.Agent{ID: "agt_stop", Name: "stop", Status: store.StatusStopped, ContainerID: id1, DindContainerID: id2})
	h := newTestHandler(mgr, fake)

	rec := doJSON(t, h, "GET", "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	type resources struct {
		CPUPercent    float64 `json:"cpu_percent"`
		MemUsedBytes  uint64  `json:"mem_used_bytes"`
		MemLimitBytes uint64  `json:"mem_limit_bytes"`
		Pids          uint64  `json:"pids"`
	}
	var resp struct {
		Agents []struct {
			ID        string     `json:"id"`
			Resources *resources `json:"resources"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Fatalf("agents = %d, want 2; body: %s", len(resp.Agents), rec.Body)
	}
	for _, a := range resp.Agents {
		switch a.ID {
		case "agt_run":
			if a.Resources == nil {
				t.Fatalf("running agent resources = nil, want the aggregated sample; body: %s", rec.Body)
			}
			if a.Resources.CPUPercent != 42.5 {
				t.Errorf("cpu_percent = %v, want 42.5 (summed)", a.Resources.CPUPercent)
			}
			if a.Resources.MemUsedBytes != 340<<20 {
				t.Errorf("mem_used_bytes = %d, want %d (summed)", a.Resources.MemUsedBytes, uint64(340<<20))
			}
			if a.Resources.MemLimitBytes != 2<<30 {
				t.Errorf("mem_limit_bytes = %d, want %d (the MAX, not the sum)", a.Resources.MemLimitBytes, uint64(2<<30))
			}
			if a.Resources.Pids != 7 {
				t.Errorf("pids = %d, want 7 (summed)", a.Resources.Pids)
			}
		case "agt_stop":
			if a.Resources != nil {
				t.Errorf("stopped agent resources = %+v, want the key ABSENT", a.Resources)
			}
		default:
			t.Errorf("unexpected agent id %q", a.ID)
		}
	}

	// Only the running agent's two containers may be read: 2 calls, not 4.
	statsCalls := 0
	for _, c := range fake.Calls() {
		if c.Op == dockerclienttest.OpContainerStats {
			statsCalls++
		}
	}
	if statsCalls != 2 {
		t.Errorf("ContainerStats ran %d times, want 2 (only the running agent's two containers)", statsCalls)
	}
}

// TestHandleList_ResourcesPartialFailure proves the all-or-nothing rule: when
// EITHER container's stats read fails, the agent's resources key is absent
// entirely -- a half total is exactly the misleading number the field must
// never show -- while the rest of the response stays a normal 200. The
// failure is FailOnce, so only the FIRST read (the agent container) fails and
// the dind read SUCCEEDS: an implementation that kept summing the reads that
// worked would surface the sidecar-only total, and this test catches it.
func TestHandleList_ResourcesPartialFailure(t *testing.T) {
	fake := dockerclienttest.New()
	ctx := context.Background()

	id, err := fake.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "agent", Image: "agent:dev"})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := fake.ContainerStart(ctx, id); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	dindID, err := fake.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "dind", Image: "docker:27-dind"})
	if err != nil {
		t.Fatalf("ContainerCreate(dind): %v", err)
	}
	if err := fake.ContainerStart(ctx, dindID); err != nil {
		t.Fatalf("ContainerStart(dind): %v", err)
	}
	if err := fake.SetStats(dindID, dockerclient.Stats{Read: time.Now(), CPUPercent: 32.5, MemoryUsed: 240 << 20, MemoryLimit: 2 << 30, Pids: 5}); err != nil {
		t.Fatalf("SetStats(dind): %v", err)
	}

	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_run", Name: "run", Status: store.StatusRunning, ContainerID: id, DindContainerID: dindID})
	h := newTestHandler(mgr, fake)
	fake.FailOnce(dockerclienttest.OpContainerStats, errors.New("boom"))

	rec := doJSON(t, h, "GET", "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Agents []struct {
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Resources json.RawMessage `json:"resources"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	if len(resp.Agents) != 1 {
		t.Fatalf("agents = %d, want 1; body: %s", len(resp.Agents), rec.Body)
	}
	if resp.Agents[0].Resources != nil {
		t.Errorf("resources = %s, want the key ABSENT when either read fails (not the sidecar-only partial)", resp.Agents[0].Resources)
	}
	if resp.Agents[0].Name != "run" {
		t.Errorf("name = %q, want %q (the rest of the view stays intact)", resp.Agents[0].Name, "run")
	}
	// The read must have been attempted at all (the nil is a discarded
	// partial, not a skip): at least one stats call, and the failing one is
	// the agent container's -- the fake consumes FailOnce on the first call,
	// which agentResources makes against ids[0] = the agent container.
	var firstStatsCall string
	for _, c := range fake.Calls() {
		if c.Op == dockerclienttest.OpContainerStats {
			firstStatsCall = c.Target
			break
		}
	}
	if firstStatsCall != id {
		t.Errorf("first ContainerStats target = %q, want the agent container %q (so the dind leg is the one that would have succeeded)", firstStatsCall, id)
	}
}

// TestHandleList_ResourcesMissingDind pins the other half of the all-or-nothing
// rule: a running agent whose record lacks a dind container ID (only a legacy
// or corrupted record can) reports NO resources rather than a silently
// under-reported agent-container-only total.
func TestHandleList_ResourcesMissingDind(t *testing.T) {
	fake := dockerclienttest.New()
	ctx := context.Background()

	id, err := fake.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "agent", Image: "agent:dev"})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := fake.ContainerStart(ctx, id); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	if err := fake.SetStats(id, dockerclient.Stats{Read: time.Now(), CPUPercent: 10.0, MemoryUsed: 100 << 20, MemoryLimit: 1 << 30, Pids: 2}); err != nil {
		t.Fatalf("SetStats: %v", err)
	}

	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_run", Name: "run", Status: store.StatusRunning, ContainerID: id})
	h := newTestHandler(mgr, fake)

	rec := doJSON(t, h, "GET", "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Agents []struct {
			ID        string          `json:"id"`
			Resources json.RawMessage `json:"resources"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	if len(resp.Agents) != 1 {
		t.Fatalf("agents = %d, want 1; body: %s", len(resp.Agents), rec.Body)
	}
	if resp.Agents[0].Resources != nil {
		t.Errorf("resources = %s, want the key ABSENT when the dind container ID is missing (no one-container partial)", resp.Agents[0].Resources)
	}
	// And it must not have attempted any read at all.
	for _, c := range fake.Calls() {
		if c.Op == dockerclienttest.OpContainerStats {
			t.Errorf("ContainerStats was called for target %q; want no stats read without both container IDs", c.Target)
		}
	}
}

// --- disk usage (#219) ------------------------------------------------------

// diskBody decodes one agent's "disk" object. Every component is a *int64 so a
// PRESENT 0 (a real reading -- a never-written volume) is distinguishable from
// an OMITTED key (unknown).
type diskBody struct {
	WorkspaceBytes    *int64 `json:"workspace_bytes"`
	ClaudeConfigBytes *int64 `json:"claude_config_bytes"`
	DindCacheBytes    *int64 `json:"dind_cache_bytes"`
	FileStoreBytes    *int64 `json:"file_store_bytes"`
	TotalBytes        *int64 `json:"total_bytes"`
	CollectedAt       string `json:"collected_at"`
}

// decodeDiskList runs GET /api/agents and returns each agent's disk object
// keyed by id -- nil when that agent carries no disk key.
func decodeDiskList(t *testing.T, h http.Handler) map[string]*diskBody {
	t.Helper()
	rec := doJSON(t, h, "GET", "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Agents []struct {
			ID   string    `json:"id"`
			Disk *diskBody `json:"disk"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	out := make(map[string]*diskBody, len(resp.Agents))
	for _, a := range resp.Agents {
		out[a.ID] = a.Disk
	}
	return out
}

// seedVolumes creates the named volumes on the fake and seeds each usage.
func seedVolumes(t *testing.T, fake *dockerclienttest.Fake, usage map[string]int64) {
	t.Helper()
	ctx := context.Background()
	for name, size := range usage {
		if _, err := fake.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: name}); err != nil {
			t.Fatalf("VolumeCreate(%s): %v", name, err)
		}
		if err := fake.SetVolumeUsage(name, size); err != nil {
			t.Fatalf("SetVolumeUsage(%s): %v", name, err)
		}
	}
}

// countOp returns how many recorded calls used op.
func countOp(fake *dockerclienttest.Fake, op dockerclienttest.Op) int {
	n := 0
	for _, c := range fake.Calls() {
		if c.Op == op {
			n++
		}
	}
	return n
}

// TestHandleList_DiskUsage is the #219 regression test: a RUNNING and a STOPPED
// agent BOTH carry a disk object -- a stopped agent still owns its volumes and
// its file-store subtree, which is the whole point of the feature -- with the
// four components resolved by the record's OWN volume names, the file-store
// walk summed, the total added up, and collected_at populated. The contrast
// with TestHandleList_Resources (where a stopped agent has NO resources key) is
// deliberate.
func TestHandleList_DiskUsage(t *testing.T) {
	fake := dockerclienttest.New()
	seedVolumes(t, fake, map[string]int64{
		"ws-run":  0, // a real 0: a never-written volume
		"cc-run":  12 << 10,
		"dc-run":  5 << 30,
		"ws-stop": 3 << 20,
		"cc-stop": 4 << 20,
		"dc-stop": 0, // unseeded volumes also read as 0
	})

	fs := newFilestore(t)
	for id, content := range map[string]string{"agt_run": "hello world", "agt_stop": "stop!"} {
		if err := fs.EnsureAgentDir(id); err != nil {
			t.Fatalf("EnsureAgentDir(%s): %v", id, err)
		}
		if _, err := fs.Save("agents/"+id+"/f", strings.NewReader(content), 1<<20); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
	}

	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_run", Name: "run", Status: store.StatusRunning, ContainerID: "cid",
		WorkspaceVolume: "ws-run", ClaudeConfigVolume: "cc-run", DindCacheVolume: "dc-run"})
	mgr.seed(store.Agent{ID: "agt_stop", Name: "stop", Status: store.StatusStopped,
		WorkspaceVolume: "ws-stop", ClaudeConfigVolume: "cc-stop", DindCacheVolume: "dc-stop"})
	h := newTestHandlerFiles(mgr, fake, fs, 0)

	got := decodeDiskList(t, h)

	run := got["agt_run"]
	if run == nil {
		t.Fatalf("running agent has no disk object; got %+v", got)
	}
	// workspace_bytes: 0 must be PRESENT (a real reading), not an omitted key.
	if run.WorkspaceBytes == nil || *run.WorkspaceBytes != 0 {
		t.Errorf("workspace_bytes = %v, want a PRESENT 0 (a real reading, not omitted)", run.WorkspaceBytes)
	}
	if run.ClaudeConfigBytes == nil || *run.ClaudeConfigBytes != 12<<10 {
		t.Errorf("claude_config_bytes = %v, want %d", run.ClaudeConfigBytes, 12<<10)
	}
	if run.DindCacheBytes == nil || *run.DindCacheBytes != 5<<30 {
		t.Errorf("dind_cache_bytes = %v, want %d", run.DindCacheBytes, 5<<30)
	}
	if run.FileStoreBytes == nil || *run.FileStoreBytes != int64(len("hello world")) {
		t.Errorf("file_store_bytes = %v, want %d (the walked subtree)", run.FileStoreBytes, len("hello world"))
	}
	wantRunTotal := int64(0 + 12<<10 + 5<<30 + len("hello world"))
	if run.TotalBytes == nil || *run.TotalBytes != wantRunTotal {
		t.Errorf("total_bytes = %v, want %d", run.TotalBytes, wantRunTotal)
	}
	if run.CollectedAt == "" {
		t.Errorf("collected_at is empty, want the snapshot time")
	}

	stop := got["agt_stop"]
	if stop == nil {
		t.Fatalf("STOPPED agent has no disk object; a stopped agent still owns its volumes and files: %+v", got)
	}
	wantStopTotal := int64(3<<20 + 4<<20 + 0 + len("stop!"))
	if stop.TotalBytes == nil || *stop.TotalBytes != wantStopTotal {
		t.Errorf("stopped total_bytes = %v, want %d", stop.TotalBytes, wantStopTotal)
	}
}

// TestHandleList_DiskUsageOneDfCall is the acceptance criterion made
// non-vacuous by the call counter: two GETs (as the 3s UI poll produces),
// N agents, and exactly ONE system/df call. The TTL is far above this test's
// duration, so the second request must be served from the cache.
func TestHandleList_DiskUsageOneDfCall(t *testing.T) {
	fake := dockerclienttest.New()
	seedVolumes(t, fake, map[string]int64{"ws-1": 1, "cc-1": 2, "dc-1": 3, "ws-2": 4, "cc-2": 5, "dc-2": 6})

	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Name: "one", Status: store.StatusStopped,
		WorkspaceVolume: "ws-1", ClaudeConfigVolume: "cc-1", DindCacheVolume: "dc-1"})
	mgr.seed(store.Agent{ID: "agt_2", Name: "two", Status: store.StatusStopped,
		WorkspaceVolume: "ws-2", ClaudeConfigVolume: "cc-2", DindCacheVolume: "dc-2"})
	h := newTestHandler(mgr, fake)

	decodeDiskList(t, h)
	decodeDiskList(t, h)

	if n := countOp(fake, dockerclienttest.OpVolumeUsage); n != 1 {
		t.Errorf("VolumeUsage ran %d times across two GETs, want 1 (the TTL must serve the cached snapshot)", n)
	}
}

// TestHandleList_DiskUsageRefreshesAfterTTL shortens the TTL to 0 (always
// stale, and deterministically so -- no sleep), so a second GET must recompute
// and issue a second df call.
func TestHandleList_DiskUsageRefreshesAfterTTL(t *testing.T) {
	old := diskUsageTTL
	diskUsageTTL = 0
	t.Cleanup(func() { diskUsageTTL = old })

	fake := dockerclienttest.New()
	seedVolumes(t, fake, map[string]int64{"ws-1": 1, "cc-1": 2, "dc-1": 3})
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Name: "one", Status: store.StatusStopped,
		WorkspaceVolume: "ws-1", ClaudeConfigVolume: "cc-1", DindCacheVolume: "dc-1"})
	h := newTestHandler(mgr, fake)

	decodeDiskList(t, h)
	decodeDiskList(t, h)

	if n := countOp(fake, dockerclienttest.OpVolumeUsage); n != 2 {
		t.Errorf("VolumeUsage ran %d times after the TTL expired, want 2", n)
	}
}

// TestHandleList_DiskUsageMissingVolume: a record naming a volume the daemon
// does not report (removed mid-delete, or a legacy record naming a volume that
// never existed) drops only THAT component; the total is then absent (a half
// total is the misleading number the field must never show), and the response
// is still a clean 200.
func TestHandleList_DiskUsageMissingVolume(t *testing.T) {
	fake := dockerclienttest.New()
	// ws-1 and cc-1 exist; dc-1 is deliberately NOT created.
	seedVolumes(t, fake, map[string]int64{"ws-1": 7, "cc-1": 8})

	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Name: "one", Status: store.StatusStopped,
		WorkspaceVolume: "ws-1", ClaudeConfigVolume: "cc-1", DindCacheVolume: "dc-1"})
	h := newTestHandler(mgr, fake)

	got := decodeDiskList(t, h)["agt_1"]
	if got == nil {
		t.Fatal("agent has no disk object, want the known components")
	}
	if got.WorkspaceBytes == nil || *got.WorkspaceBytes != 7 {
		t.Errorf("workspace_bytes = %v, want 7", got.WorkspaceBytes)
	}
	if got.ClaudeConfigBytes == nil || *got.ClaudeConfigBytes != 8 {
		t.Errorf("claude_config_bytes = %v, want 8", got.ClaudeConfigBytes)
	}
	if got.DindCacheBytes != nil {
		t.Errorf("dind_cache_bytes = %v, want ABSENT (the volume is not on the daemon)", got.DindCacheBytes)
	}
	if got.TotalBytes != nil {
		t.Errorf("total_bytes = %v, want ABSENT (all-or-nothing with a missing component)", got.TotalBytes)
	}
}

// TestHandleList_DiskUsageDfFailure: disk discovery being down must never fail
// the list. The df call fails, so no agent carries a disk key, but the rest of
// the response is intact and the status is 200.
func TestHandleList_DiskUsageDfFailure(t *testing.T) {
	fake := dockerclienttest.New()
	seedVolumes(t, fake, map[string]int64{"ws-1": 7, "cc-1": 8, "dc-1": 9})

	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Name: "one", Status: store.StatusStopped,
		WorkspaceVolume: "ws-1", ClaudeConfigVolume: "cc-1", DindCacheVolume: "dc-1"})
	h := newTestHandler(mgr, fake)
	fake.Fail(dockerclienttest.OpVolumeUsage, errors.New("daemon down"))

	rec := doJSON(t, h, "GET", "/api/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when disk discovery fails; body: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Agents []struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Disk json.RawMessage `json:"disk"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v", err)
	}
	if len(resp.Agents) != 1 {
		t.Fatalf("agents = %d, want 1; body: %s", len(resp.Agents), rec.Body)
	}
	if resp.Agents[0].Disk != nil {
		t.Errorf("disk = %s, want the key ABSENT when the df call fails", resp.Agents[0].Disk)
	}
	if resp.Agents[0].Name != "one" {
		t.Errorf("name = %q, want the rest of the view intact", resp.Agents[0].Name)
	}
}

// TestHandleList_DiskUsageDfFailureThrottles: a boot-time df failure (no
// snapshot to fall back on) must not be retried on every 3s poll -- each retry
// blocks up to diskUsageReadTimeout. Two GETs within one TTL, df failing
// throughout, produce exactly ONE attempt. lastTry is zero at boot, so the
// very first attempt is not throttled; only the ones after it are.
func TestHandleList_DiskUsageDfFailureThrottles(t *testing.T) {
	fake := dockerclienttest.New()
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Name: "one", Status: store.StatusStopped,
		WorkspaceVolume: "ws-1", ClaudeConfigVolume: "cc-1", DindCacheVolume: "dc-1"})
	h := newTestHandler(mgr, fake)
	fake.Fail(dockerclienttest.OpVolumeUsage, errors.New("daemon down"))

	decodeDiskList(t, h) // first attempt, not throttled (lastTry is zero)
	decodeDiskList(t, h) // within the TTL: must be served the failure, not retried

	if n := countOp(fake, dockerclienttest.OpVolumeUsage); n != 1 {
		t.Errorf("VolumeUsage was attempted %d times across two GETs after a failure, want 1 (the failure must throttle to one attempt per TTL)", n)
	}
}

// TestHandleList_DiskUsageFilestoreDisabled: with the file store disabled
// (nil), file_store_bytes is absent everywhere -- but the total is STILL
// present, because the file-store component does not APPLY rather than being
// unknown. The total is the three volumes.
func TestHandleList_DiskUsageFilestoreDisabled(t *testing.T) {
	fake := dockerclienttest.New()
	seedVolumes(t, fake, map[string]int64{"ws-1": 7, "cc-1": 8, "dc-1": 9})

	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Name: "one", Status: store.StatusStopped,
		WorkspaceVolume: "ws-1", ClaudeConfigVolume: "cc-1", DindCacheVolume: "dc-1"})
	h := newTestHandler(mgr, fake) // nil file store

	got := decodeDiskList(t, h)["agt_1"]
	if got == nil {
		t.Fatal("agent has no disk object, want the volume components")
	}
	if got.FileStoreBytes != nil {
		t.Errorf("file_store_bytes = %v, want ABSENT with the file store disabled", got.FileStoreBytes)
	}
	if got.TotalBytes == nil || *got.TotalBytes != 7+8+9 {
		t.Errorf("total_bytes = %v, want %d (inapplicable, not unknown)", got.TotalBytes, 7+8+9)
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

func TestAgentInfo(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{
		ID:     "agt_a",
		Name:   "a",
		Status: store.StatusRunning,
		Repo:   "acme/widget.git",
	})
	mgr.defaultRefs[config.HarnessClaudeCode] = "ghcr.io/example/agent:1.2.3"
	mgr.dockerRuntime = "crun"
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_a/info", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	var resp agentInfoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding info response: %v", err)
	}
	if resp.Agent.ID != "agt_a" || resp.Agent.Repo != "acme/widget.git" || resp.Agent.Status != store.StatusRunning {
		t.Errorf("agent = %+v, want the seeded record with its resolved create-time parameters", resp.Agent)
	}
	if resp.Operator.AgentImage != "ghcr.io/example/agent:1.2.3" || resp.Operator.DockerRuntime != "crun" {
		t.Errorf("operator = %+v, want the manager's agent image and docker runtime", resp.Operator)
	}
}

// TestAgentInfo_PerHarnessDefaultImage proves handleAgentInfo asks the
// manager for the DEFAULT IMAGE OF THE AGENT'S OWN HARNESS, not a single
// operator-wide value: an opencode agent's info response must carry the
// opencode default, not claude-code's.
func TestAgentInfo_PerHarnessDefaultImage(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_cc", Name: "cc", Status: store.StatusRunning, Harness: config.HarnessClaudeCode})
	mgr.seed(store.Agent{ID: "agt_oc", Name: "oc", Status: store.StatusRunning, Harness: config.HarnessOpenCode})
	mgr.defaultRefs[config.HarnessClaudeCode] = "ghcr.io/example/agent:1.2.3"
	mgr.defaultRefs[config.HarnessOpenCode] = "ghcr.io/example/agent-opencode:9.9.9"
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_cc/info", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	var resp agentInfoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding info response: %v", err)
	}
	if resp.Operator.AgentImage != "ghcr.io/example/agent:1.2.3" {
		t.Errorf("claude-code agent's operator.agent_image = %q, want the claude-code default", resp.Operator.AgentImage)
	}

	rec = doJSON(t, h, "GET", "/api/agents/agt_oc/info", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	resp = agentInfoResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding info response: %v", err)
	}
	if resp.Operator.AgentImage != "ghcr.io/example/agent-opencode:9.9.9" {
		t.Errorf("opencode agent's operator.agent_image = %q, want the opencode-specific default", resp.Operator.AgentImage)
	}
}

func TestAgentInfo_NotFound(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_missing/info", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeNotFound {
		t.Errorf("error code = %q, want %q", got, CodeNotFound)
	}
}

func TestAgentInfo_MethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_a", Name: "a"})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_a/info", map[string]string{"name": "x"})

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusMethodNotAllowed, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeMethodNotAllowed {
		t.Errorf("error code = %q, want %q", got, CodeMethodNotAllowed)
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

// --- POST /api/agents/{id}/update -----------------------------------------

func TestHandleUpdate_OK(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Name: "old", Status: store.StatusRunning})
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_1/update", updateAgentRequest{
		createAgentRequest: createAgentRequest{Name: "new", Backend: "ollama"},
		ImageTag:           "20260101-000000",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
	}
	if a := decodeAgent(t, rec); a.Name != "new" || a.Status != store.StatusRunning {
		t.Errorf("updated agent = %+v, want Name=new Status=running", a)
	}
	if len(mgr.updateReqs) != 1 || mgr.updateReqs[0].ImageTag != "20260101-000000" {
		t.Errorf("updateReqs = %+v, want one call carrying the image tag", mgr.updateReqs)
	}
}

func TestHandleUpdate_NotFound(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_missing/update", updateAgentRequest{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body)
	}
}

func TestHandleUpdate_NotUpdatable(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Status: store.StatusCreating})
	mgr.updateErr = fmt.Errorf("updating agent %q: %w", "agt_1", agent.ErrNotUpdatable)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_1/update", updateAgentRequest{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeNotUpdatable {
		t.Errorf("error code = %q, want %q", got, CodeNotUpdatable)
	}
}

// TestHandleUpdate_OperationInFlight proves an update refused because another
// operation already holds the agent maps to a 409 with its own code, distinct
// from the not-updatable one.
func TestHandleUpdate_OperationInFlight(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Status: store.StatusRunning})
	mgr.updateErr = fmt.Errorf("updating agent %q: %w", "agt_1", agent.ErrOperationInFlight)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_1/update", updateAgentRequest{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeOperationInFlight {
		t.Errorf("error code = %q, want %q", got, CodeOperationInFlight)
	}
}

// TestHandleDelete_OperationInFlight proves a delete refused because another
// operation already holds the agent maps to a 409 rather than the default 500.
func TestHandleDelete_OperationInFlight(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Status: store.StatusDeleting})
	mgr.deleteErr = fmt.Errorf("deleting agent %q: %w", "agt_1", agent.ErrOperationInFlight)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "DELETE", "/api/agents/agt_1", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeOperationInFlight {
		t.Errorf("error code = %q, want %q", got, CodeOperationInFlight)
	}
}

func TestHandleUpdate_InvalidBackend(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.seed(store.Agent{ID: "agt_1", Status: store.StatusRunning})
	mgr.updateErr = errors.New("Update must not be reached")
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents/agt_1/update", updateAgentRequest{
		createAgentRequest: createAgentRequest{Backend: "gpt"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
	}
	env := decodeEnvelope(t, rec)
	if env.Error.Code != CodeInvalidParam || env.Error.Field != "backend" {
		t.Errorf("error = %+v, want code %q field %q", env.Error, CodeInvalidParam, "backend")
	}
}

func TestHandleUpdate_InvalidHarness(t *testing.T) {
	t.Run("harness=opencode with backend=anthropic is a 400 before the manager is called", func(t *testing.T) {
		mgr := newFakeManager(5)
		mgr.seed(store.Agent{ID: "agt_1", Status: store.StatusRunning})
		mgr.updateErr = errors.New("Update must not be reached")
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "POST", "/api/agents/agt_1/update", updateAgentRequest{
			createAgentRequest: createAgentRequest{Harness: "opencode", Backend: "anthropic"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
		}
		env := decodeEnvelope(t, rec)
		if env.Error.Code != CodeInvalidParam || env.Error.Field != "harness" {
			t.Errorf("error = %+v, want code %q field %q", env.Error, CodeInvalidParam, "harness")
		}
	})

	t.Run("a valid update with harness=opencode and backend=ollama reaches the manager", func(t *testing.T) {
		mgr := newFakeManager(5)
		mgr.seed(store.Agent{ID: "agt_1", Status: store.StatusRunning})
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "POST", "/api/agents/agt_1/update", updateAgentRequest{
			createAgentRequest: createAgentRequest{Harness: "opencode", Backend: "ollama"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body)
		}
		if len(mgr.updateReqs) != 1 || mgr.updateReqs[0].Harness != "opencode" {
			t.Errorf("updateReqs = %+v, want one call carrying harness=opencode", mgr.updateReqs)
		}
	})
}

func TestHandleUpdate_BadJSON(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	r := httptest.NewRequest("POST", "/api/agents/agt_1/update", bytes.NewReader([]byte(`{`)))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeBadJSON {
		t.Errorf("error code = %q, want %q", got, CodeBadJSON)
	}
}

func TestHandleUpdate_UnknownField(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	r := httptest.NewRequest("POST", "/api/agents/agt_1/update", bytes.NewReader([]byte(`{"bogus":1}`)))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body)
	}
}

func TestHandleUpdate_MethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agents/agt_1/update", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusMethodNotAllowed, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodeMethodNotAllowed {
		t.Errorf("error code = %q, want %q", got, CodeMethodNotAllowed)
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

func TestDelete_PurgeFiles(t *testing.T) {
	t.Run("no flag: files are not purged", func(t *testing.T) {
		mgr := newFakeManager(5)
		mgr.seed(store.Agent{ID: "x"})
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "DELETE", "/api/agents/x", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
		}
		if len(mgr.purgedIDs) != 0 {
			t.Errorf("purgedIDs = %v, want empty", mgr.purgedIDs)
		}
	})

	t.Run("purge_files=true: PurgeAgentFiles is called", func(t *testing.T) {
		mgr := newFakeManager(5)
		mgr.seed(store.Agent{ID: "x"})
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "DELETE", "/api/agents/x?purge_files=true", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
		}
		if len(mgr.purgedIDs) != 1 || mgr.purgedIDs[0] != "x" {
			t.Errorf("purgedIDs = %v, want [x]", mgr.purgedIDs)
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte(`"files_purged":true`)) {
			t.Errorf("body = %s, want files_purged:true", rec.Body.String())
		}
	})

	t.Run("purge_files=nope: 400 invalid_param", func(t *testing.T) {
		mgr := newFakeManager(5)
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "DELETE", "/api/agents/x?purge_files=nope", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
		}
		env := decodeEnvelope(t, rec)
		if env.Error.Code != CodeInvalidParam || env.Error.Field != "purge_files" {
			t.Errorf("error = %+v, want invalid_param/purge_files", env.Error)
		}
	})

	t.Run("purge error: 500", func(t *testing.T) {
		mgr := newFakeManager(5)
		mgr.seed(store.Agent{ID: "x"})
		mgr.purgeErr = errors.New("disk on fire")
		h := newTestHandler(mgr, dockerclienttest.New())

		rec := doJSON(t, h, "DELETE", "/api/agents/x?purge_files=1", nil)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
		}
	})
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
