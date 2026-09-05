package wsbridge

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
)

// tmuxShellBoot starts a real tmux server with a "main" session running a
// plain shell, then sleeps -- everything the terminal bridge needs and
// nothing else. Using a bare alpine+tmux container here (rather than the
// full agent image) keeps these tests independent of tmux-boot.sh and of
// rebuilding the agent image, since this issue is about the generic
// `docker exec ... tmux attach-session -t main` bridge, not about the agent
// image specifically -- that coupling is already covered by
// internal/agent's and this package's other integration tests.
const tmuxShellBoot = `apk add -q tmux >/dev/null && tmux new-session -d -s main sh && sleep 600`

// newTerminalTestServer starts a real tmux+shell container and an
// httptest.Server serving only the terminal route, wired to the real Docker
// client. Returns the server, the agent id the route expects, and the
// underlying container id (for tests that need to act on the container
// directly, e.g. stopping it).
func newTerminalTestServer(t *testing.T, c dockerclient.Client) (srv *httptest.Server, agentID, containerID string) {
	t.Helper()
	ctx := context.Background()
	name := "docker-operator-itest-terminal-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	id, err := c.ContainerCreate(ctx, dockerclient.ContainerSpec{
		Name:  name,
		Image: "alpine:3",
		Cmd:   []string{"sh", "-c", tmuxShellBoot},
	})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		_ = c.ContainerStop(context.Background(), id, 5*time.Second)
		_ = c.ContainerRemove(context.Background(), id)
	})
	if err := c.ContainerStart(ctx, id); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	// Wait for tmux's session to exist before wiring the route to it.
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, code, err := runExec(ctx, c, id, []string{"tmux", "has-session", "-t", tmuxSession})
		if err == nil && code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tmux %q session never appeared in container %s (last: exit=%d err=%v)", tmuxSession, id, code, err)
		}
		time.Sleep(300 * time.Millisecond)
	}

	agentID = "agt_terminal_itest"
	mux := newTestMux(fakeGetter{agentID: {ID: agentID, ContainerID: id}}, c)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, agentID, id
}

// dialTerminal opens a WebSocket to the given test server's terminal route.
func dialTerminal(t *testing.T, srv *httptest.Server, agentID string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/agents/" + agentID + "/terminal"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		status := "no response"
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dialing %s: %v (%s)", wsURL, err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readUntil reads binary frames off conn, accumulating them, until buf
// contains want or the deadline passes.
func readUntil(t *testing.T, conn *websocket.Conn, want string, timeout time.Duration) []byte {
	t.Helper()
	var buf bytes.Buffer
	deadline := time.Now().Add(timeout)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			t.Fatalf("SetReadDeadline: %v", err)
		}
		mt, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage (accumulated so far: %q): %v", buf.String(), err)
		}
		if mt == websocket.BinaryMessage {
			buf.Write(data)
			if bytes.Contains(buf.Bytes(), []byte(want)) {
				return buf.Bytes()
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q; accumulated: %q", want, buf.String())
		}
	}
}

// TestIntegrationTerminalConnectTypeObserve is #72's first acceptance
// criterion: connect, type, and observe the output -- for real, over an
// actual WebSocket into an actual docker exec.
func TestIntegrationTerminalConnectTypeObserve(t *testing.T) {
	c := realDockerClient(t)
	srv, agentID, _ := newTerminalTestServer(t, c)
	conn := dialTerminal(t, srv, agentID)

	const marker = "connect-type-observe-8f3a"
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo "+marker+"\n")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	out := readUntil(t, conn, marker, 20*time.Second)
	t.Logf("PASS: observed %d bytes of pane output containing the marker: %q", len(out), out)
}

// TestIntegrationTerminalReconnectShowsScrollback is #72's second acceptance
// criterion: disconnecting and reconnecting shows the same tmux session's
// content, proving reattach (not a fresh session each time).
func TestIntegrationTerminalReconnectShowsScrollback(t *testing.T) {
	c := realDockerClient(t)
	srv, agentID, _ := newTerminalTestServer(t, c)

	first := dialTerminal(t, srv, agentID)
	const marker = "reconnect-scrollback-2b91"
	if err := first.WriteMessage(websocket.BinaryMessage, []byte("echo "+marker+"\n")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	readUntil(t, first, marker, 20*time.Second)

	// Close only the WebSocket -- the container and its tmux session are
	// untouched. This is the whole point being tested: closing a WS must not
	// tear down the session.
	if err := first.Close(); err != nil {
		t.Fatalf("closing the first connection: %v", err)
	}

	// A brief pause is not required for correctness (a fresh exec attach is
	// synchronous), but keeps this test from racing the server's own cleanup
	// goroutines for the first connection.
	time.Sleep(500 * time.Millisecond)

	second := dialTerminal(t, srv, agentID)
	// tmux repaints the CURRENT screen the instant a new client attaches --
	// no further input is needed. The marker must still be on screen because
	// nothing was written to produce enough lines to scroll it off.
	out := readUntil(t, second, marker, 20*time.Second)
	t.Logf("PASS: reconnecting attached to the SAME tmux session and observed its repainted screen containing the marker: %q", out)
}

// TestIntegrationTerminalCleanFailureOnStoppedContainer is #72's third
// acceptance criterion: stopping the container mid-session must not hang or
// panic the bridge, and must close the WebSocket cleanly so the client can
// show "agent is not running" instead of spinning forever.
func TestIntegrationTerminalCleanFailureOnStoppedContainer(t *testing.T) {
	c := realDockerClient(t)
	srv, agentID, containerID := newTerminalTestServer(t, c)
	conn := dialTerminal(t, srv, agentID)

	// Prove the session is live before pulling the rug out.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo before-stop\n")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	readUntil(t, conn, "before-stop", 20*time.Second)

	// Stop the container directly (not through Delete -- this test is about
	// the bridge's reaction to the container disappearing out from under it,
	// independent of the agent lifecycle).
	if err := c.ContainerStop(context.Background(), containerID, 5*time.Second); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}

	// The bridge must notice (its exec stream ends) and close the WebSocket
	// on its own, within a bounded time -- not hang until the test's own
	// timeout, and not panic (a panic in the handler goroutine would surface
	// as this whole test process crashing, not as a Go test failure, which is
	// exactly why this assertion matters more than it looks like it does).
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	var closeErr error
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			closeErr = err
			break
		}
	}
	if !websocket.IsCloseError(closeErr, websocket.CloseNormalClosure) {
		t.Fatalf("ReadMessage error after the container stopped = %v, want a normal-closure close error (a clean failure, not a hang or a raw connection reset)", closeErr)
	}
	t.Logf("PASS: the bridge closed the websocket cleanly (%v) once the container stopped mid-session", closeErr)
}
