package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

func TestHandleAgentImageTags_OK(t *testing.T) {
	mgr := newFakeManager(5)
	checked := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	mgr.imageTags = store.AgentImageTags{
		Tags:      []string{"20260910-070000", "20260909-120000"},
		CheckedAt: checked,
	}
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agent-image/tags", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var resp agentImageTagsResponse
	decode(t, rec.Body.Bytes(), &resp)
	if len(resp.Tags) != 2 || resp.Tags[0] != "20260910-070000" {
		t.Errorf("Tags = %v, want the stored list newest-first", resp.Tags)
	}
	if resp.Newest != "20260910-070000" {
		t.Errorf("Newest = %q, want %q", resp.Newest, "20260910-070000")
	}
	if resp.OperatorDefault != "latest" {
		t.Errorf("OperatorDefault = %q, want %q (the tag of the operator's AgentImage)", resp.OperatorDefault, "latest")
	}
	if resp.CheckedAt == nil || !resp.CheckedAt.Equal(checked) {
		t.Errorf("CheckedAt = %v, want %s", resp.CheckedAt, checked)
	}
	if resp.LastError != "" {
		t.Errorf("LastError = %q, want empty", resp.LastError)
	}
}

func TestHandleAgentImageTags_Empty(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "GET", "/api/agent-image/tags", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var resp agentImageTagsResponse
	decode(t, rec.Body.Bytes(), &resp)
	if resp.Tags == nil {
		t.Error("Tags = nil, want an empty JSON array")
	}
	if len(resp.Tags) != 0 || resp.Newest != "" {
		t.Errorf("resp = %+v, want empty tags and no newest", resp)
	}
	if resp.CheckedAt != nil {
		t.Errorf("CheckedAt = %v, want nil before the first refresh", resp.CheckedAt)
	}
}

func TestHandleAgentImageRefresh_OK(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.imageTags = store.AgentImageTags{Tags: []string{"20260910-070000"}, CheckedAt: time.Now().UTC()}
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agent-image/refresh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if mgr.refreshCalls != 1 {
		t.Errorf("refreshCalls = %d, want 1", mgr.refreshCalls)
	}
	var resp agentImageTagsResponse
	decode(t, rec.Body.Bytes(), &resp)
	if len(resp.Tags) != 1 {
		t.Errorf("Tags = %v, want the read-back list", resp.Tags)
	}
}

func TestHandleAgentImageRefresh_PollErrorStillReturns200(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.refreshErr = agent.ErrInvalidImageTag // any error; the handler must not propagate it
	mgr.imageTags = store.AgentImageTags{
		Tags:      []string{"20260910-070000"},
		CheckedAt: time.Now().UTC(),
		LastError: "registry is unreachable",
	}
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agent-image/refresh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 despite the poll error; body: %s", rec.Code, rec.Body)
	}
	if mgr.refreshCalls != 1 {
		t.Errorf("refreshCalls = %d, want 1", mgr.refreshCalls)
	}
	var resp agentImageTagsResponse
	decode(t, rec.Body.Bytes(), &resp)
	if resp.LastError != "registry is unreachable" {
		t.Errorf("LastError = %q, want it surfaced in the body", resp.LastError)
	}
}

func TestHandleAgentImageRefresh_MethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "GET", "/api/agent-image/refresh", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/agent-image/refresh status = %d, want 405", rec.Code)
	}
}

func TestHandleCreate_InvalidImageTag(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.createErr = agent.ErrInvalidImageTag
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agents", map[string]any{"image_tag": "bad tag!!"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
	}
	env := decodeEnvelope(t, rec)
	if env.Error.Code != CodeInvalidParam || env.Error.Field != "image_tag" {
		t.Errorf("error = %+v, want invalid_param on field \"image_tag\"", env.Error)
	}
}
