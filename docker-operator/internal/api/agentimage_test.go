package api

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/agent"
	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
)

func TestHandleAgentImageTags_BothHarnesses(t *testing.T) {
	mgr := newFakeManager(5)
	checked := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)

	cc := fakeInventory(config.HarnessClaudeCode,
		[]string{"20260910-070000", "20260909-120000"},
		[]string{"20260909-120000"})
	cc.CheckedAt = checked
	mgr.setInventory(config.HarnessClaudeCode, cc)

	oc := fakeInventory(config.HarnessOpenCode, []string{"20260905-120000"}, nil)
	oc.CheckedAt = checked
	oc.LastError = "registry is unreachable"
	mgr.setInventory(config.HarnessOpenCode, oc)

	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "GET", "/api/agent-image/tags", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var resp agentImageTagsResponse
	decode(t, rec.Body.Bytes(), &resp)

	if len(resp.Harnesses) != len(config.Harnesses()) {
		t.Fatalf("harnesses = %v, want one entry per config.Harnesses()", resp.Harnesses)
	}
	got := resp.Harnesses[config.HarnessClaudeCode]
	if got.Repo != "ghcr.io/psenna/ai-sandbox-agent" {
		t.Errorf("claude-code repo = %q", got.Repo)
	}
	if got.Newest != "20260910-070000" {
		t.Errorf("claude-code newest = %q, want the newest published tag", got.Newest)
	}
	if got.DefaultTag != "20260909-120000" {
		t.Errorf("claude-code default_tag = %q, want the newest LOCAL tag", got.DefaultTag)
	}
	if len(got.Tags) == 0 || got.Tags[0].Tag != "20260910-070000" || got.Tags[0].Present {
		t.Errorf("claude-code tags[0] = %+v, want the newest published tag, not present", got.Tags)
	}
	var present bool
	for _, o := range got.Tags {
		if o.Tag == "20260909-120000" {
			present = o.Present
		}
	}
	if !present {
		t.Errorf("claude-code tags = %+v, want 20260909-120000 marked present", got.Tags)
	}
	if got.CheckedAt == nil || !got.CheckedAt.Equal(checked) {
		t.Errorf("claude-code checked_at = %v, want %s", got.CheckedAt, checked)
	}
	if got.LastError != "" {
		t.Errorf("claude-code last_error = %q, want empty", got.LastError)
	}

	oc2 := resp.Harnesses[config.HarnessOpenCode]
	if oc2.Repo != "ghcr.io/psenna/ai-sandbox-agent-opencode" {
		t.Errorf("opencode repo = %q, want the opencode repository", oc2.Repo)
	}
	if oc2.Newest != "20260905-120000" {
		t.Errorf("opencode newest = %q, want its OWN newest tag", oc2.Newest)
	}
	if oc2.LastError != "registry is unreachable" {
		t.Errorf("opencode last_error = %q, want it surfaced per harness", oc2.LastError)
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
	for _, harness := range config.Harnesses() {
		got, ok := resp.Harnesses[harness]
		if !ok {
			t.Fatalf("harness %q missing before the first refresh; want an entry for every harness", harness)
		}
		if got.Tags == nil {
			t.Errorf("harness %q tags = nil, want an empty JSON array", harness)
		}
		if len(got.Tags) != 0 || got.Newest != "" {
			t.Errorf("harness %q = %+v, want empty tags and no newest", harness, got)
		}
		if got.CheckedAt != nil {
			t.Errorf("harness %q checked_at = %v, want nil before the first refresh", harness, got.CheckedAt)
		}
	}
}

func TestHandleAgentImageRefresh_OK(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.setInventory(config.HarnessClaudeCode, fakeInventory(config.HarnessClaudeCode, []string{"20260910-070000"}, nil))
	mgr.setInventory(config.HarnessOpenCode, fakeInventory(config.HarnessOpenCode, []string{"20260905-120000"}, nil))
	h := newTestHandler(mgr, dockerclienttest.New())

	rec := doJSON(t, h, "POST", "/api/agent-image/refresh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	for _, harness := range config.Harnesses() {
		if mgr.refreshCalls[harness] != 1 {
			t.Errorf("refreshCalls[%q] = %d, want 1 (every harness is polled)", harness, mgr.refreshCalls[harness])
		}
	}
	var resp agentImageTagsResponse
	decode(t, rec.Body.Bytes(), &resp)
	if len(resp.Harnesses[config.HarnessClaudeCode].Tags) != 1 ||
		len(resp.Harnesses[config.HarnessOpenCode].Tags) != 1 {
		t.Errorf("harnesses = %+v, want both read-back lists", resp.Harnesses)
	}
}

// TestHandleAgentImageRefresh_OneHarnessFailsOtherSucceeds proves the
// refresh loop polls EVERY harness even when one poll fails, and surfaces
// the failure on that harness alone. It runs the failure on EACH harness in
// turn on purpose: failing only the LAST harness of config.Harnesses() would
// pass just as well against a loop that gave up on the first error (break or
// return), which would silently leave every later harness un-refreshed.
func TestHandleAgentImageRefresh_OneHarnessFailsOtherSucceeds(t *testing.T) {
	const pollErr = "registry is unreachable"
	for _, failing := range config.Harnesses() {
		t.Run(failing, func(t *testing.T) {
			mgr := newFakeManager(5)
			mgr.refreshErr[failing] = errors.New(pollErr)
			for _, harness := range config.Harnesses() {
				inv := fakeInventory(harness, []string{"20260910-070000"}, nil)
				if harness == failing {
					// The real Manager keeps the last-known list and
					// records the poll error in the snapshot.
					inv.LastError = pollErr
				}
				mgr.setInventory(harness, inv)
			}

			h := newTestHandler(mgr, dockerclienttest.New())
			rec := doJSON(t, h, "POST", "/api/agent-image/refresh", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 despite the poll error; body: %s", rec.Code, rec.Body)
			}
			for _, harness := range config.Harnesses() {
				if mgr.refreshCalls[harness] != 1 {
					t.Errorf("refreshCalls[%q] = %d, want 1: harness %q's failure must not skip any other harness's poll",
						harness, mgr.refreshCalls[harness], failing)
				}
			}
			var resp agentImageTagsResponse
			decode(t, rec.Body.Bytes(), &resp)
			for _, harness := range config.Harnesses() {
				got := resp.Harnesses[harness]
				want := ""
				if harness == failing {
					want = pollErr
				}
				if got.LastError != want {
					t.Errorf("harness %q last_error = %q, want %q (the failure belongs to harness %q alone)",
						harness, got.LastError, want, failing)
				}
				if got.Newest != "20260910-070000" {
					t.Errorf("harness %q newest = %q, want the read-back list even when its own poll failed",
						harness, got.Newest)
				}
			}
		})
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

// --- image tag manager -------------------------------------------------------

func TestHandleAgentImageTagDelete_OK(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.setInventory(config.HarnessClaudeCode,
		fakeInventory(config.HarnessClaudeCode, []string{"20260910-070000"}, []string{"20260910-070000", "20260909-120000"}))

	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "DELETE", "/api/agent-image/tags/claude-code/20260909-120000", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if mgr.deleteTagCalls["claude-code|20260909-120000"] != 1 {
		t.Errorf("deleteTagCalls = %v, want exactly one call", mgr.deleteTagCalls)
	}
	// The read-back map is the report: the deleted tag must be gone from it.
	var resp agentImageTagsResponse
	decode(t, rec.Body.Bytes(), &resp)
	for _, o := range resp.Harnesses[config.HarnessClaudeCode].Tags {
		if o.Tag == "20260909-120000" {
			t.Errorf("read-back tags still contain the deleted tag: %+v", resp.Harnesses[config.HarnessClaudeCode].Tags)
		}
	}
}

func TestHandleAgentImageTagDelete_InUse(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.deleteTagErr["claude-code|20260910-070000"] = agent.ErrImageTagInUse

	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "DELETE", "/api/agent-image/tags/claude-code/20260910-070000", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rec.Code, rec.Body)
	}
	if env := decodeEnvelope(t, rec); env.Error.Code != CodeImageTagInUse {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeImageTagInUse)
	}
}

func TestHandleAgentImageTagDelete_Missing(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.deleteTagErr["claude-code|nope"] = agent.ErrImageTagMissing

	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "DELETE", "/api/agent-image/tags/claude-code/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body)
	}
	if env := decodeEnvelope(t, rec); env.Error.Code != CodeNotFound {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeNotFound)
	}
}

func TestHandleAgentImageTagDelete_InvalidTag(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.deleteTagErr["claude-code|bad tag!"] = agent.ErrInvalidImageTag

	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "DELETE", "/api/agent-image/tags/claude-code/bad%20tag!", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
	}
}

func TestHandleAgentImageTagDelete_MethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "GET", "/api/agent-image/tags/claude-code/20260910-070000", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on the tag path status = %d, want 405", rec.Code)
	}
}

func TestHandleAgentImageCleanup_BothHarnesses(t *testing.T) {
	mgr := newFakeManager(5)
	for _, harness := range config.Harnesses() {
		mgr.cleanupReports[harness] = agent.AgentImageCleanupReport{
			Harness: harness, Removed: []string{"20260909-120000"},
		}
		mgr.setInventory(harness, fakeInventory(harness, []string{"20260910-070000"}, nil))
	}

	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "POST", "/api/agent-image/cleanup", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var resp agentImageCleanupResponse
	decode(t, rec.Body.Bytes(), &resp)
	if len(resp.Report) != len(config.Harnesses()) {
		t.Fatalf("report = %+v, want one entry per harness", resp.Report)
	}
	for _, rep := range resp.Report {
		if mgr.cleanupCalls[rep.Harness] != 1 {
			t.Errorf("cleanupCalls[%q] = %d, want 1", rep.Harness, mgr.cleanupCalls[rep.Harness])
		}
		if len(resp.Harnesses[rep.Harness].Tags) == 0 {
			t.Errorf("harnesses[%q] = empty, want the refreshed map alongside the report", rep.Harness)
		}
	}
}

func TestHandleAgentImageCleanup_OneHarnessFailsOtherStillCleans(t *testing.T) {
	// Run the failure on each harness in turn, so a give-up-on-first-error
	// loop cannot pass: both harnesses must always be attempted.
	for _, failing := range config.Harnesses() {
		t.Run(failing, func(t *testing.T) {
			mgr := newFakeManager(5)
			mgr.cleanupErr[failing] = errors.New("store is closed")
			for _, harness := range config.Harnesses() {
				mgr.setInventory(harness, fakeInventory(harness, []string{"20260910-070000"}, nil))
			}

			h := newTestHandler(mgr, dockerclienttest.New())
			rec := doJSON(t, h, "POST", "/api/agent-image/cleanup", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 -- one harness's failure must not fail the request; body: %s", rec.Code, rec.Body)
			}
			var resp agentImageCleanupResponse
			decode(t, rec.Body.Bytes(), &resp)
			for _, rep := range resp.Report {
				if rep.Harness == failing {
					if rep.Error == "" {
						t.Errorf("harness %q report.Error = \"\", want the failure surfaced in the report", failing)
					}
				} else if rep.Error != "" {
					t.Errorf("harness %q report.Error = %q, want empty -- the failure belongs to %q alone", rep.Harness, rep.Error, failing)
				}
			}
			for _, harness := range config.Harnesses() {
				if mgr.cleanupCalls[harness] != 1 {
					t.Errorf("cleanupCalls[%q] = %d, want 1: %q's failure must not skip the other harness",
						harness, mgr.cleanupCalls[harness], failing)
				}
			}
		})
	}
}

func TestHandleAgentImageCleanup_MethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "GET", "/api/agent-image/cleanup", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/agent-image/cleanup status = %d, want 405", rec.Code)
	}
}

func TestHandleAgentImagePullLatest_BothHarnesses(t *testing.T) {
	mgr := newFakeManager(5)
	for _, harness := range config.Harnesses() {
		mgr.pullReports[harness] = agent.AgentImagePullReport{Harness: harness, Tag: "20260910-070000"}
	}

	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "POST", "/api/agent-image/pull-latest", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var resp agentImagePullResponse
	decode(t, rec.Body.Bytes(), &resp)
	if len(resp.Report) != len(config.Harnesses()) {
		t.Fatalf("report = %+v, want one entry per harness", resp.Report)
	}
	for _, rep := range resp.Report {
		if rep.Tag != "20260910-070000" {
			t.Errorf("harness %q tag = %q, want the pulled tag", rep.Harness, rep.Tag)
		}
		if mgr.pullCalls[rep.Harness] != 1 {
			t.Errorf("pullCalls[%q] = %d, want 1", rep.Harness, mgr.pullCalls[rep.Harness])
		}
	}
}

func TestHandleAgentImagePullLatest_OneHarnessFailsOtherStillPulls(t *testing.T) {
	// Same each-harness-in-turn shape as the cleanup test above: the pull
	// report carries errors, so a failing harness must still answer 200 and
	// never skip the other.
	for _, failing := range config.Harnesses() {
		t.Run(failing, func(t *testing.T) {
			mgr := newFakeManager(5)
			mgr.pullReports[failing] = agent.AgentImagePullReport{
				Harness: failing, Error: "registry is unreachable",
			}
			for _, harness := range config.Harnesses() {
				mgr.setInventory(harness, fakeInventory(harness, []string{"20260910-070000"}, nil))
			}

			h := newTestHandler(mgr, dockerclienttest.New())
			rec := doJSON(t, h, "POST", "/api/agent-image/pull-latest", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
			}
			var resp agentImagePullResponse
			decode(t, rec.Body.Bytes(), &resp)
			for _, rep := range resp.Report {
				if rep.Harness == failing {
					if rep.Error == "" || rep.Tag != "" {
						t.Errorf("harness %q report = %+v, want the error and no tag", failing, rep)
					}
				} else if rep.Error != "" {
					t.Errorf("harness %q report.Error = %q, want empty -- the failure belongs to %q alone", rep.Harness, rep.Error, failing)
				}
			}
			for _, harness := range config.Harnesses() {
				if mgr.pullCalls[harness] != 1 {
					t.Errorf("pullCalls[%q] = %d, want 1: %q's failure must not skip the other harness",
						harness, mgr.pullCalls[harness], failing)
				}
			}
		})
	}
}

func TestHandleAgentImagePullLatest_MethodNotAllowed(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, dockerclienttest.New())
	rec := doJSON(t, h, "GET", "/api/agent-image/pull-latest", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/agent-image/pull-latest status = %d, want 405", rec.Code)
	}
}
