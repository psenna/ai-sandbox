// Package wsbridge owns every exec-based interaction with a running agent's
// tmux session: the browser terminal bridge (a WebSocket pumped into
// `docker exec ... tmux attach -t main`) and the programmatic read-back of
// the tmux pipe-pane capture log that tmux-boot.sh writes to the agent's own
// workspace volume.
//
// Output capture (#70) is implemented: ReadOutput execs `tail`/`cat` against
// OutputLogPath inside an agent container and returns the captured bytes, so
// the operator can read an agent's terminal output without a browser
// attached and without depending on tmux's own bounded scrollback.
// internal/api's GET /api/agents/{id}/output (#71) is the first caller.
//
// The terminal bridge itself -- the WebSocket handler, the TTY exec and its
// resize control frames -- is still pending (#72).
package wsbridge
