package wsbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// discardLog is a real, non-nil *slog.Logger that writes nowhere. slog's own
// methods dereference their receiver's handler, so a literal nil
// *slog.Logger (unlike NewTerminalHandler's own nil-defaulting) panics.
var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeGetter is a minimal AgentGetter for handler tests that never need the
// full bridge (the pre-upgrade error paths return before touching Docker at
// all).
type fakeGetter map[string]store.Agent

func (f fakeGetter) Get(_ context.Context, id string) (store.Agent, error) {
	a, ok := f[id]
	if !ok {
		return store.Agent{}, fmt.Errorf("getting agent %q: %w", id, store.ErrNotFound)
	}
	return a, nil
}

func newTestMux(getter AgentGetter, docker dockerclient.ExecClient) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws/agents/{id}/terminal", NewTerminalHandler(getter, docker, nil))
	return mux
}

// --- pre-upgrade error paths (no WebSocket dial needed: these return before
// the handler ever calls Upgrade) --------------------------------------------

func TestTerminal_AgentNotFound(t *testing.T) {
	h := newTestMux(fakeGetter{}, dockerclienttest.New())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ws/agents/agt_missing/terminal", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body)
	}
}

func TestTerminal_NoContainerYet(t *testing.T) {
	h := newTestMux(fakeGetter{"agt_a": {ID: "agt_a", Status: store.StatusCreating}}, dockerclienttest.New())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ws/agents/agt_a/terminal", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusServiceUnavailable, rec.Body)
	}
}

// --- handleControl (pure logic, no WebSocket or real exec needed) -----------

func TestHandleControl_Resize(t *testing.T) {
	docker := dockerclienttest.New()
	ctx := context.Background()
	cid, err := docker.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "c", Image: "i"})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	execID, err := docker.ExecCreate(ctx, cid, dockerclient.ExecSpec{Cmd: []string{"tmux", "attach-session", "-t", "main"}})
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}

	handleControl(ctx, docker, execID, "agt_a", []byte(`{"type":"resize","cols":80,"rows":24}`), discardLog)

	found := false
	for _, c := range docker.Calls() {
		if c.Op == dockerclienttest.OpExecResize && c.Target == execID {
			found = true
		}
	}
	if !found {
		t.Errorf("Calls() = %v, want an ExecResize on %q", docker.Calls(), execID)
	}
}

func TestHandleControl_IgnoresBadInput(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"malformed json", `not json`},
		{"unknown type", `{"type":"ping"}`},
		{"zero cols", `{"type":"resize","cols":0,"rows":24}`},
		{"negative rows", `{"type":"resize","cols":80,"rows":-1}`},
		{"cols too large", `{"type":"resize","cols":999999,"rows":24}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docker := dockerclienttest.New()
			ctx := context.Background()
			cid, err := docker.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: "c", Image: "i"})
			if err != nil {
				t.Fatalf("ContainerCreate: %v", err)
			}
			execID, err := docker.ExecCreate(ctx, cid, dockerclient.ExecSpec{Cmd: []string{"tmux"}})
			if err != nil {
				t.Fatalf("ExecCreate: %v", err)
			}

			// Must not panic, and must not call ExecResize.
			handleControl(ctx, docker, execID, "agt_a", []byte(tc.data), discardLog)

			for _, c := range docker.Calls() {
				if c.Op == dockerclienttest.OpExecResize {
					t.Errorf("Calls() = %v, want no ExecResize for input %q", docker.Calls(), tc.data)
				}
			}
		})
	}
}

// TestContainerTerminalHandler_ClosesCleanlyWhenContainerAbsent proves the
// Anthropic-login terminal route (NewContainerTerminalHandler) does not hang
// or 500 when its fixed container is not there -- it upgrades, then closes
// the WebSocket with the not-ready reason.
func TestContainerTerminalHandler_ClosesCleanlyWhenContainerAbsent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws/anthropic/login/terminal",
		NewContainerTerminalHandler(dockerclienttest.New(), "docker-operator-anthropic-login", "no login in progress", discardLog))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/anthropic/login/terminal"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()

	var ce *websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("ReadMessage err = %v, want a *websocket.CloseError (a clean close, not a hang or a raw read error)", err)
	}
	if !strings.Contains(ce.Text, "no login in progress") {
		t.Errorf("close reason = %q, want it to mention the not-ready reason", ce.Text)
	}
}
