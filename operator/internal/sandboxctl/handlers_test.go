package sandboxctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// fakeStore is a minimal, non-concurrent-safe-beyond-a-mutex Store double
// for exercising the HTTP handlers without envtest.
type fakeStore struct {
	mu sync.Mutex

	snap Snapshot

	declareErr error
	declared   *WaitProbe

	reportErr  error
	idempotent bool
	reported   *Result
}

func (f *fakeStore) Snapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeStore) Refresh(context.Context) error { return nil }

func (f *fakeStore) DeclareWait(_ context.Context, p WaitProbe, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.declareErr != nil {
		return f.declareErr
	}
	f.declared = &p
	return nil
}

func (f *fakeStore) ReportDone(_ context.Context, r Result, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reportErr != nil {
		return false, f.reportErr
	}
	f.reported = &r
	return f.idempotent, nil
}

func (f *fakeStore) RecordSnapshotAttempt(context.Context, SnapshotAttempt, time.Time) error {
	return nil
}

func (f *fakeStore) RecordSnapshot(context.Context, SnapshotRecord, time.Time) error {
	return nil
}

func newTestServer(t *testing.T, store *fakeStore, poll *Poller) *httptest.Server {
	t.Helper()
	if poll == nil {
		poll = NewPoller(store, time.Second, nil, nil)
	}
	env := EnvironmentRef{Name: "test-env", Namespace: "test-ns"}
	cfg := Config{Listen: "127.0.0.1:0"}
	srv := NewServer(cfg, store, poll, env, time.Now, nil)
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func postJSON(t *testing.T, ts *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(b)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeEnvelope(t *testing.T, resp *http.Response) ErrorEnvelope {
	t.Helper()
	var env ErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decoding error envelope: %v", err)
	}
	return env
}

func TestHandleWait_HappyPath(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(t, store, nil)

	resp := postJSON(t, ts, "/v1/wait", WaitRequest{Type: v1alpha1.WaitTypeNotBefore, Reason: "cooldown", Params: map[string]string{"duration": "1h"}})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var wr WaitResponse
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if wr.Type != v1alpha1.WaitTypeNotBefore || wr.Environment != "test-env" {
		t.Errorf("unexpected response: %+v", wr)
	}
	if store.declared == nil || store.declared.Type != v1alpha1.WaitTypeNotBefore {
		t.Errorf("store.declared = %+v, want NotBefore probe recorded", store.declared)
	}
}

func TestHandleWait_ValidationFailure(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(t, store, nil)

	resp := postJSON(t, ts, "/v1/wait", WaitRequest{Type: "SolarEclipse", Reason: "nope"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error.Code != CodeUnknownProbeType {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeUnknownProbeType)
	}
}

func TestHandleWait_AlreadyDeclared(t *testing.T) {
	store := &fakeStore{declareErr: ErrWaitAlreadyDeclared}
	ts := newTestServer(t, store, nil)

	resp := postJSON(t, ts, "/v1/wait", WaitRequest{Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{"duration": "1h"}})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error.Code != CodeWaitAlreadyDeclared {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeWaitAlreadyDeclared)
	}
}

func TestHandleWait_StatusPatchFailed(t *testing.T) {
	store := &fakeStore{declareErr: fmt.Errorf("boom: unreachable API server")}
	ts := newTestServer(t, store, nil)

	resp := postJSON(t, ts, "/v1/wait", WaitRequest{Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{"duration": "1h"}})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error.Code != CodeStatusPatchFailed {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeStatusPatchFailed)
	}
}

func TestHandleDone_HappyPathThenIdempotentRepeat(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(t, store, nil)

	resp := postJSON(t, ts, "/v1/done", DoneRequest{Outcome: "success", Message: "all good"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first report status = %d, want 202", resp.StatusCode)
	}

	store.idempotent = true
	resp2 := postJSON(t, ts, "/v1/done", DoneRequest{Outcome: "success", Message: "all good"})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("idempotent repeat status = %d, want 200", resp2.StatusCode)
	}
}

func TestHandleDone_DifferingRepeatConflicts(t *testing.T) {
	store := &fakeStore{reportErr: ErrResultAlreadyReported}
	ts := newTestServer(t, store, nil)

	resp := postJSON(t, ts, "/v1/done", DoneRequest{Outcome: "failure", Message: "changed my mind"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error.Code != CodeResultAlreadyReported {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeResultAlreadyReported)
	}
}

func TestHandleDone_InvalidOutcome(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(t, store, nil)

	resp := postJSON(t, ts, "/v1/done", DoneRequest{Outcome: "maybe"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error.Code != CodeInvalidOutcome {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeInvalidOutcome)
	}
}

func TestHandleProgress_HappyPath(t *testing.T) {
	store := &fakeStore{}
	poll := NewPoller(store, time.Second, nil, nil)
	ts := newTestServer(t, store, poll)

	resp := postJSON(t, ts, "/v1/progress", ProgressRequest{Message: "cloned repo"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if got := poll.Progress(); len(got) != 1 || got[0] != "cloned repo" {
		t.Errorf("Progress() = %v, want [\"cloned repo\"]", got)
	}
}

func TestFreezing_RejectsAllThreeMutatingEndpoints(t *testing.T) {
	store := &fakeStore{}
	poll := NewPoller(store, time.Second, nil, nil)
	poll.LatchFreezing(context.Background())
	ts := newTestServer(t, store, poll)

	cases := []struct {
		path string
		body any
	}{
		{"/v1/wait", WaitRequest{Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{"duration": "1h"}}},
		{"/v1/done", DoneRequest{Outcome: "success"}},
		{"/v1/progress", ProgressRequest{Message: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp := postJSON(t, ts, tc.path, tc.body)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409", resp.StatusCode)
			}
			env := decodeEnvelope(t, resp)
			if env.Error.Code != CodeFreezing {
				t.Errorf("code = %q, want %q", env.Error.Code, CodeFreezing)
			}
		})
	}

	// /v1/status still reports freezing=true rather than rejecting.
	resp, err := http.Get(ts.URL + "/v1/status") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /v1/status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var sr StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !sr.Freezing {
		t.Error("StatusResponse.Freezing = false, want true")
	}

	// /healthz keeps answering.
	healthResp, err := http.Get(ts.URL + "/healthz") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = healthResp.Body.Close() }()
	if healthResp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200 even while freezing", healthResp.StatusCode)
	}
}

func TestHandleStatus_ServedFromCacheAndStaleness(t *testing.T) {
	store := &fakeStore{snap: Snapshot{Phase: v1alpha1.PhaseRunning, Fresh: true, ObservedAt: time.Now()}}
	poll := NewPoller(store, time.Second, nil, nil)
	ts := newTestServer(t, store, poll)

	resp, err := http.Get(ts.URL + "/v1/status") //nolint:noctx
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sr StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sr.Stale {
		t.Error("Stale = true, want false for a fresh, recent snapshot")
	}
	if sr.Phase != string(v1alpha1.PhaseRunning) {
		t.Errorf("Phase = %q, want Running", sr.Phase)
	}

	// Now go stale: observedAt far in the past.
	store.mu.Lock()
	store.snap.ObservedAt = time.Now().Add(-time.Hour)
	store.mu.Unlock()

	resp2, err := http.Get(ts.URL + "/v1/status") //nolint:noctx
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	var sr2 StatusResponse
	if err := json.NewDecoder(resp2.Body).Decode(&sr2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !sr2.Stale {
		t.Error("Stale = false, want true for an old snapshot")
	}
}

func TestHandleHealthz(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(t, store, nil)
	resp, err := http.Get(ts.URL + "/healthz") //nolint:noctx
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
