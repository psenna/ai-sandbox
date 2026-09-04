// Package wsbridge bridges a browser WebSocket to an agent's interactive
// tmux session inside its container (docker exec ... tmux attach), and
// exposes the tmux pipe-pane capture log so the operator can read an
// agent's terminal output programmatically rather than only proxying it
// live.
//
// Scaffold only (issue #61); the bridge and the output capture land in a
// later task.
package wsbridge
