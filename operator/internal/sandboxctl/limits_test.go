package sandboxctl

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestLimitBody_OversizeRejectedWith413(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(t, store, nil)

	body, err := json.Marshal(WaitRequest{
		Type:   v1alpha1.WaitTypeNotBefore,
		Reason: "x",
		Params: map[string]string{"duration": strings.Repeat("1", 17<<10)},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(body) <= maxWaitDoneBodyBytes {
		t.Fatalf("test body is %d bytes, want > %d", len(body), maxWaitDoneBodyBytes)
	}

	resp, err := http.Post(ts.URL+"/v1/wait", "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error.Code != CodePayloadTooLarge {
		t.Errorf("code = %q, want %q", env.Error.Code, CodePayloadTooLarge)
	}
}

func TestLimitBody_ExactlyAtLimitIsNotRejectedForSize(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(t, store, nil)

	base, err := json.Marshal(WaitRequest{Type: v1alpha1.WaitTypeNotBefore, Reason: "", Params: map[string]string{"duration": "1h"}})
	if err != nil {
		t.Fatalf("marshal base: %v", err)
	}
	overhead := len(base)
	if overhead >= maxWaitDoneBodyBytes {
		t.Fatalf("base overhead %d already exceeds the limit %d; adjust test", overhead, maxWaitDoneBodyBytes)
	}
	padLen := maxWaitDoneBodyBytes - overhead
	body, err := json.Marshal(WaitRequest{Type: v1alpha1.WaitTypeNotBefore, Reason: strings.Repeat("x", padLen), Params: map[string]string{"duration": "1h"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(body) != maxWaitDoneBodyBytes {
		t.Fatalf("constructed body is %d bytes, want exactly %d", len(body), maxWaitDoneBodyBytes)
	}

	resp, err := http.Post(ts.URL+"/v1/wait", "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		t.Errorf("status = 413 for a body exactly at the limit, want it to pass the size check (may still 400 on other validation)")
	}
}

func TestUnknownTopLevelField_Returns400UnknownField(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(t, store, nil)

	resp, err := http.Post(ts.URL+"/v1/wait", "application/json", //nolint:noctx
		bytes.NewReader([]byte(`{"type":"NotBefore","reason":"x","params":{"duration":"1h"},"bogus":true}`)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error.Code != CodeUnknownField {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeUnknownField)
	}
}

func TestWrongContentType_Returns415(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(t, store, nil)

	resp, err := http.Post(ts.URL+"/v1/wait", "text/plain", bytes.NewReader([]byte("hello"))) //nolint:noctx
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
}

func TestWrongMethod_Returns405(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(t, store, nil)

	resp, err := http.Get(ts.URL + "/v1/wait") //nolint:noctx
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestUnknownPath_Returns404JSON(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(t, store, nil)

	resp, err := http.Get(ts.URL + "/nope") //nolint:noctx
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error.Code != CodeNotFound {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeNotFound)
	}
}

func TestRateLimit_BurstPlusOneIs429(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(t, store, nil)

	req := WaitRequest{Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{"duration": "1h"}}
	var last *http.Response
	for i := 0; i < waitDoneBurst; i++ {
		resp := postJSON(t, ts, "/v1/wait", req)
		if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusConflict {
			t.Fatalf("request %d: status = %d, want 202 (or 409 from the fake store), not rate-limited yet", i, resp.StatusCode)
		}
		last = resp
	}
	_ = last

	resp := postJSON(t, ts, "/v1/wait", req)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("burst+1 request: status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}
	env := decodeEnvelope(t, resp)
	if env.Error.Code != CodeRateLimited {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeRateLimited)
	}
}
