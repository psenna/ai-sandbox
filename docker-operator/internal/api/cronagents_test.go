package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// decodeInto decodes rec's JSON body into v. There is no existing
// generic response-decoding helper in this package: templates_test.go's
// decodeEnvelope is specific to ErrorEnvelope and decodeTemplateList to
// templateListResponse, so this is a new helper (not a second one of an
// existing kind) added because cronagents_test.go needs to decode several
// different response types.
func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding response from %q: %v", rec.Body.String(), err)
	}
}

func seedTemplateForCronAgent(t *testing.T, mgr *fakeManager) store.Template {
	t.Helper()
	tmpl, err := mgr.CreateTemplate(nil, store.TemplateCreateSpec{Name: "base config", Harness: "claude-code"})
	if err != nil {
		t.Fatalf("seeding a template: %v", err)
	}
	return tmpl
}

func TestCreateCronAgent(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)

	rec := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{
		Name: "nightly", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Enabled: true, Task: "go", OnComplete: "stop",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var ca store.CronAgent
	decodeInto(t, rec, &ca)
	if ca.Name != "nightly" || ca.TemplateID != tmpl.ID || ca.Schedule != "0 9 * * *" {
		t.Errorf("created cron agent = %+v, want the submitted fields", ca)
	}
	if got := rec.Header().Get("Location"); got != "/api/cron-agents/"+ca.ID {
		t.Errorf("Location header = %q, want %q", got, "/api/cron-agents/"+ca.ID)
	}
}

func TestCreateCronAgent_MissingName(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	rec := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "stop"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestCreateCronAgent_UnknownTemplateRejected(t *testing.T) {
	h := newTestHandler(newFakeManager(5), nil)
	rec := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{
		Name: "nightly", TemplateID: "tpl_missing", Schedule: "0 9 * * *", Task: "go", OnComplete: "stop",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (template_id does not reference an existing template)", rec.Code, http.StatusBadRequest)
	}
}

func TestCreateCronAgent_MalformedCronRejected(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	rec := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{
		Name: "nightly", TemplateID: tmpl.ID, Schedule: "not a cron expression", Task: "go", OnComplete: "stop",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestCreateCronAgent_UnknownOnCompleteRejected(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	rec := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{
		Name: "nightly", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "nonsense",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestCreateCronAgent_OpencodeTemplateRejected(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl, err := mgr.CreateTemplate(nil, store.TemplateCreateSpec{Name: "opencode base", Harness: "opencode"})
	if err != nil {
		t.Fatalf("seeding an opencode template: %v", err)
	}
	rec := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{
		Name: "nightly", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "stop",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (scheduling is claude-code only)", rec.Code, http.StatusBadRequest)
	}
}

func TestListCronAgents(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{Name: "one", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "stop"})
	doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{Name: "two", TemplateID: tmpl.ID, Schedule: "0 10 * * *", Task: "go", OnComplete: "stop"})

	rec := doJSON(t, h, "GET", "/api/cron-agents", nil)
	var body cronAgentListResponse
	decodeInto(t, rec, &body)
	if len(body.CronAgents) != 2 {
		t.Errorf("CronAgents = %+v, want 2", body.CronAgents)
	}
}

func TestUpdateCronAgent(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	created := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{Name: "original", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "stop"})
	var ca store.CronAgent
	decodeInto(t, created, &ca)

	rec := doJSON(t, h, "PUT", "/api/cron-agents/"+ca.ID, cronAgentRequest{
		Name: "renamed", TemplateID: tmpl.ID, Schedule: "0 10 * * *", Enabled: true, Task: "go now", OnComplete: "delete",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var updated store.CronAgent
	decodeInto(t, rec, &updated)
	if updated.Name != "renamed" || updated.Schedule != "0 10 * * *" || updated.OnComplete != "delete" {
		t.Errorf("updated cron agent = %+v, want the new fields", updated)
	}
}

func TestUpdateCronAgent_NotFound(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	rec := doJSON(t, h, "PUT", "/api/cron-agents/ca_missing", cronAgentRequest{
		Name: "x", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "stop",
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestDeleteCronAgent(t *testing.T) {
	mgr := newFakeManager(5)
	h := newTestHandler(mgr, nil)
	tmpl := seedTemplateForCronAgent(t, mgr)
	created := doJSON(t, h, "POST", "/api/cron-agents", cronAgentRequest{Name: "t", TemplateID: tmpl.ID, Schedule: "0 9 * * *", Task: "go", OnComplete: "stop"})
	var ca store.CronAgent
	decodeInto(t, created, &ca)

	rec := doJSON(t, h, "DELETE", "/api/cron-agents/"+ca.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	list := doJSON(t, h, "GET", "/api/cron-agents", nil)
	var body cronAgentListResponse
	decodeInto(t, list, &body)
	if len(body.CronAgents) != 0 {
		t.Errorf("CronAgents after delete = %+v, want empty", body.CronAgents)
	}
}

func TestDeleteCronAgent_IsIdempotent(t *testing.T) {
	h := newTestHandler(newFakeManager(5), nil)
	rec := doJSON(t, h, "DELETE", "/api/cron-agents/ca_never_existed", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (idempotent)", rec.Code, http.StatusOK)
	}
}

func TestHandleListCronAgentScheduleRuns(t *testing.T) {
	mgr := newFakeManager(5)
	mgr.scheduleRuns = map[string][]store.ScheduleRun{
		"ca_1": {{ID: "run_1", CronAgentID: "ca_1", AgentID: "agt_1", Status: store.ScheduleRunStatusSuccess}},
	}
	h := newTestHandler(mgr, nil)
	rec := doJSON(t, h, "GET", "/api/cron-agents/ca_1/runs", nil)
	var body scheduleRunListResponse
	decodeInto(t, rec, &body)
	if len(body.Runs) != 1 || body.Runs[0].ID != "run_1" {
		t.Errorf("Runs = %+v, want [run_1]", body.Runs)
	}
}

func TestHandleListCronAgentScheduleRuns_EmptyForUnknownCronAgent(t *testing.T) {
	h := newTestHandler(newFakeManager(5), nil)
	rec := doJSON(t, h, "GET", "/api/cron-agents/ca_missing/runs", nil)
	var body scheduleRunListResponse
	decodeInto(t, rec, &body)
	if len(body.Runs) != 0 {
		t.Errorf("Runs = %+v, want empty for an unknown cron agent", body.Runs)
	}
}

func TestCronAgentsMethodNotAllowed(t *testing.T) {
	h := newTestHandler(newFakeManager(5), nil)
	cases := []struct{ method, path string }{
		{"DELETE", "/api/cron-agents"},
		{"PUT", "/api/cron-agents"},
		{"POST", "/api/cron-agents/ca_00000001"},
	}
	for _, c := range cases {
		rec := doJSON(t, h, c.method, c.path, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status = %d, want %d", c.method, c.path, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}
