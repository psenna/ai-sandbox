#!/bin/sh
# tmux-boot.sh -- the docker-operator agent container's foreground process.
#
# The operator overrides the agent container's Cmd to this script at
# container-create time (internal/agent's create flow), so the image itself
# stays orchestrator-agnostic: its CMD is still ["bash"] and a plain
# `docker run` of it is an ordinary interactive shell. The image's ENTRYPOINT
# (/entrypoint.sh) runs first -- it writes the git-proxy and DependaProxy
# configuration -- and ends in `exec "$@"`, which is how control reaches here.
#
# Why a tmux session rather than exec'ing `claude` directly:
#
#  1. The web UI terminal is a `docker exec ... tmux attach -t main`, so the
#     session has to outlive any single viewer (and the operator process).
#  2. The container's lifetime is tied to the tmux SESSION, not to `claude`.
#     A crashed or exited `claude` leaves a dead-but-readable pane the user can
#     reattach to and inspect, instead of taking the container -- and every
#     scrap of evidence about why it died -- down with it.
set -eu

# tmux needs a terminal type even for a detached session. The operator sets
# TERM in the container environment; defaulting it here too keeps the script
# runnable by hand inside the image.
: "${TERM:=xterm-256color}"
export TERM

SESSION=main
# Where the pane's output is captured. On the agent's OWN workspace volume, so
# it outlives the tmux server, the container and the operator process, and so
# the operator can read it back with a plain `tail` exec.
# internal/wsbridge.OutputLogPath must match this literal.
OUTPUT_LOG=/workspace/.agent-output.log

# ONE tmux invocation, deliberately. Verified against tmux 3.7c (what
# node:22-alpine ships, i.e. what the agent image has):
#
#   - `tmux set-option ...` on its own, before any server exists, FAILS with
#     "error connecting to /tmp/tmux-<uid>/default" and a non-zero exit -- under
#     `set -e` that would abort this script on line one.
#   - `tmux start-server` does not help: a server with zero sessions exits
#     immediately, so the following set-option fails identically.
#   - Setting the option AFTER new-session loses a race whenever `claude` exits
#     immediately -- which is precisely the case remain-on-exit exists to make
#     visible. A missing or instantly-crashing binary would kill the session,
#     and with it this script and the container. (Confirmed: without the option
#     in place first, an exec failure destroys the session in milliseconds.)
#
# Chaining both commands into one command list makes the client start the
# server, apply the global option, and only then spawn the pane -- no window in
# which the pane can die unprotected. Confirmed to survive both a normal exit
# (pane_dead=1, status=3) and a missing binary (pane_dead=1, status=127).
tmux set-option -g remain-on-exit on \; new-session -d -s "$SESSION" claude

# Capture everything the pane writes to a durable, unbounded file, so the
# operator can read an agent's output programmatically (internal/wsbridge's
# ReadOutput, behind GET /api/agents/{id}/output) rather than only proxying it
# live to a browser terminal. tmux's own scrollback is bounded AND dies with
# the server; this file is neither.
#
# A SEPARATE tmux invocation, deliberately: pipe-pane resolves its -t target
# when it runs, so the session must already exist -- it cannot be chained into
# the command list above the way set-option can.
#
# `-o` is tmux's "only open a pipe if one is not already open" guard (the man
# page's toggle idiom), NOT "output only" -- a common misreading. The direction
# flag is left at its default of -O: the PANE's output is piped to the
# command's stdin. Input typed by a viewer is captured too, but exactly once,
# through the pane's own terminal echo.
#
# Never fatal, hence the `||`. Verified against tmux 3.7c: pipe-pane exits 1
# with "target pane has exited" when the pane's process is already gone --
# precisely the case the remain-on-exit above exists to keep inspectable. An
# unguarded failure here would trip `set -e`, end this script, stop the
# container and destroy the very evidence remain-on-exit preserved. Output
# capture is an auxiliary feature; it must never be able to kill an agent.
tmux pipe-pane -o -t "$SESSION" "cat >> $OUTPUT_LOG" ||
	echo "tmux-boot: WARNING: pipe-pane failed, so $OUTPUT_LOG will not be written and the output endpoint will read back nothing; the session itself is unaffected"

# Keep the container alive for exactly as long as the tmux session exists.
# has-session is the loop CONDITION, so its non-zero exit once the session is
# finally gone ends the loop normally instead of tripping `set -e`; its stderr
# ("no server running on ...") is expected at that point and is discarded.
while tmux has-session -t "$SESSION" 2>/dev/null; do
	sleep 5
done

echo "tmux-boot: the $SESSION session is gone; exiting so the container stops"
