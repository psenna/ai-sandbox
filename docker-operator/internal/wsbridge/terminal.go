package wsbridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// tmuxSession is the session name tmux-boot.sh creates
// (docker-operator/agent/tmux-boot.sh) and the one this bridge attaches to.
const tmuxSession = "main"

// readBufferSize bounds one read from the exec stream per WebSocket frame.
// Terminal output arrives in small bursts; this is generous headroom, not a
// hard limit -- a burst larger than this is just sent as more frames.
const readBufferSize = 32 * 1024

// closeGrace bounds how long WriteControl waits to flush a close frame
// before giving up. The connection is going away either way; this only
// avoids blocking a goroutine exit on a wedged client.
const closeGrace = time.Second

// AgentGetter resolves an agent ID to its record. *agent.Manager satisfies
// this via the Get method added in #71; a fake for tests implements just
// this one method. Defined locally rather than reused from internal/api:
// wsbridge must not depend on internal/api, since the dependency runs the
// other way once cmd/docker-operator wires both onto one mux.
type AgentGetter interface {
	Get(ctx context.Context, id string) (store.Agent, error)
}

// controlMessage is the JSON control envelope a client sends as a WebSocket
// TEXT frame. Binary frames carry raw PTY bytes in both directions and never
// go through this type.
type controlMessage struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// controlTypeResize is the only control message type this bridge accepts
// today.
const controlTypeResize = "resize"

// maxTTYDimension is TTYSize's uint16 ceiling. Guarding against it here,
// before the conversion, is what makes the cast to uint16 below provably
// non-truncating rather than merely usually-fine.
const maxTTYDimension = 65535

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// V1 has no auth at all and is a local dev tool served on a single
	// trusted host -- there is no cross-origin credential for CheckOrigin to
	// protect. Revisit the moment this grows auth or a network-exposed
	// deployment.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// NewTerminalHandler returns the GET /ws/agents/{id}/terminal handler: one
// WebSocket per connection, bridged to `docker exec ... tmux attach-session
// -t main` inside the agent's container.
//
// Closing the WebSocket ends only the exec, never the container -- tmux
// keeps running, so a page refresh or a second tab is just a fresh exec onto
// the same session, and tmux repaints its current screen (including
// anything still in its scrollback) the moment a new attach connects. If the
// container is not reachable -- before connecting, or because it stops
// mid-session -- the handler fails cleanly: a pre-upgrade request gets a
// plain HTTP error, and a mid-session failure closes the WebSocket with an
// explanatory reason instead of hanging or panicking.
func NewTerminalHandler(getter AgentGetter, docker dockerclient.ExecClient, log *slog.Logger) http.HandlerFunc {
	if log == nil {
		log = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		a, err := getter.Get(r.Context(), id)
		if err != nil {
			if store.IsNotFound(err) {
				http.Error(w, "no such agent", http.StatusNotFound)
				return
			}
			log.Error("terminal: looking up agent failed", "agent_id", id, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if a.ContainerID == "" {
			http.Error(w, "agent is not running yet (no container)", http.StatusServiceUnavailable)
			return
		}

		serveTerminal(w, r, docker, a.ContainerID, "agent "+id, "agent is not running", log)
	}
}

// NewContainerTerminalHandler bridges a WebSocket to a `tmux attach-session
// -t main` inside a container named up front, rather than one looked up from
// an agent record -- used for the Anthropic-login helper
// (GET /ws/anthropic/login/terminal). If the container is not there (no
// login session started, or it was torn down), the WebSocket closes with
// notReadyReason instead of a pre-upgrade HTTP error, because by the time
// the browser opens this socket it has already decided a login is in
// progress.
func NewContainerTerminalHandler(docker dockerclient.ExecClient, containerName, notReadyReason string, log *slog.Logger) http.HandlerFunc {
	if log == nil {
		log = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		serveTerminal(w, r, docker, containerName, "container "+containerName, notReadyReason, log)
	}
}

// serveTerminal is the shared core of both terminal handlers: upgrade the
// connection, exec `tmux attach-session -t main` in target (a container name
// or ID), and pump bytes until either side ends. logLabel identifies the
// target in log lines; notReadyReason is the WebSocket close reason when the
// exec cannot be created (the container is stopped or absent).
func serveTerminal(w http.ResponseWriter, r *http.Request, docker dockerclient.ExecClient, target, logLabel, notReadyReason string, log *slog.Logger) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote its own HTTP response on failure; there is
		// no response left to write here.
		log.Warn("terminal: websocket upgrade failed", "target", logLabel, "error", err)
		return
	}
	defer func() { _ = conn.Close() }()

	execID, err := docker.ExecCreate(r.Context(), target, dockerclient.ExecSpec{
		Cmd:   []string{"tmux", "attach-session", "-t", tmuxSession},
		TTY:   true,
		Stdin: true,
	})
	if err != nil {
		log.Warn("terminal: exec create failed; the target container is likely stopped or absent", "target", logLabel, "error", err)
		closeWithReason(conn, notReadyReason)
		return
	}
	stream, err := docker.ExecAttach(r.Context(), execID)
	if err != nil {
		log.Warn("terminal: exec attach failed; the target container is likely stopped", "target", logLabel, "error", err)
		closeWithReason(conn, notReadyReason)
		return
	}
	defer func() { _ = stream.Close() }()

	bridge(r.Context(), conn, stream, docker, execID, logLabel, log)
}

// bridge pumps bytes in both directions until either side ends, then
// returns. It never touches the container: closing conn or stream ends only
// this exec.
func bridge(ctx context.Context, conn *websocket.Conn, stream dockerclient.ExecStream, docker dockerclient.ExecClient, execID, label string, log *slog.Logger) {
	done := make(chan struct{})
	var once sync.Once
	closeDone := func() { once.Do(func() { close(done) }) }

	// exec output -> browser.
	go func() {
		defer closeDone()
		buf := make([]byte, readBufferSize)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					log.Info("terminal: exec stream ended", "target", label, "error", err)
				}
				// A graceful close notifies the browser this session -- not
				// necessarily the agent -- has ended (the container may have
				// stopped, or tmux itself may have exited). The client
				// decides whether to show "reconnect" based on this.
				closeWithReason(conn, "session ended")
				return
			}
		}
	}()

	// browser -> exec stdin, plus resize control frames.
	go func() {
		defer closeDone()
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch mt {
			case websocket.BinaryMessage:
				if _, werr := stream.Write(data); werr != nil {
					return
				}
			case websocket.TextMessage:
				handleControl(ctx, docker, execID, label, data, log)
			}
		}
	}()

	<-done
}

// handleControl parses one TEXT frame as a controlMessage and acts on it. A
// malformed frame or an out-of-range/unknown message is logged and ignored
// rather than tearing down the connection -- a single bad control frame
// should not cost the user their terminal.
func handleControl(ctx context.Context, docker dockerclient.ExecClient, execID, label string, data []byte, log *slog.Logger) {
	var msg controlMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Warn("terminal: ignoring malformed control frame", "target", label, "error", err)
		return
	}
	if msg.Type != controlTypeResize {
		log.Warn("terminal: ignoring unknown control frame type", "target", label, "type", msg.Type)
		return
	}
	if msg.Cols <= 0 || msg.Rows <= 0 || msg.Cols > maxTTYDimension || msg.Rows > maxTTYDimension {
		log.Warn("terminal: ignoring out-of-range resize", "target", label, "cols", msg.Cols, "rows", msg.Rows)
		return
	}
	size := dockerclient.TTYSize{Cols: uint16(msg.Cols), Rows: uint16(msg.Rows)} //nolint:gosec // G115: both operands range-checked against maxTTYDimension immediately above
	if err := docker.ExecResize(ctx, execID, size); err != nil {
		log.Warn("terminal: resize failed", "target", label, "error", err)
	}
}

// closeWithReason sends a normal-closure control frame carrying reason. Best
// effort: the connection may already be half-gone, and there is nothing more
// to do about that than log it upstream.
func closeWithReason(conn *websocket.Conn, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason),
		time.Now().Add(closeGrace))
}
