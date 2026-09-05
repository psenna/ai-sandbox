// Package wsbridge owns every exec-based interaction with a running agent's
// tmux session: the browser terminal bridge (a WebSocket pumped into
// `docker exec ... tmux attach -t main`) and the programmatic read-back of
// the tmux pipe-pane capture log that tmux-boot.sh writes to the agent's own
// workspace volume.
//
// Both are implemented. ReadOutput (#70) execs `tail`/`cat` against
// OutputLogPath inside an agent container and returns the captured bytes;
// internal/api's GET /api/agents/{id}/output is its caller.
// NewTerminalHandler (#72) serves GET /ws/agents/{id}/terminal: one
// WebSocket per connection, binary frames carrying raw PTY bytes each way
// and a small JSON control envelope on TEXT frames for resize. Closing the
// WebSocket ends only the exec, never the agent's container -- tmux keeps
// running, so a refresh or a second tab just opens a fresh exec onto the
// same session and shows tmux's own repainted screen.
package wsbridge
