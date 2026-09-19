#!/bin/sh
# opencode-supervisor.sh -- the opencode sibling of
# docker-operator/agent/claude-supervisor.sh. It runs inside tmux-boot.sh's
# session in place of a bare `opencode "$@"`.
#
# Same job, same shape: tmux-boot.sh's `remain-on-exit` keeps the SESSION
# (and the container) alive when the harness dies, but it does not bring the
# harness back -- the web UI terminal would be left staring at a dead pane.
#
# THREE DELIBERATE DIVERGENCES from claude-supervisor.sh, all of them forced
# by measured opencode behaviour (opencode 1.18.x):
#
#  1. RESTART ON ANY EXIT, not just a non-zero one. `claude` reports a
#     non-zero status when it is killed, so its supervisor can treat 0 as
#     "the user meant it". opencode does not: an externally killed opencode
#     reports 0 for SIGINT and (usually) 0 or 143 for SIGTERM, so the exit
#     status carries NO intent information here. Restarting only on non-zero
#     would silently fail this script's own acceptance criterion ("kill the
#     opencode process, confirm the supervisor restarts it"). The rolling
#     restart budget below is therefore the ONLY guard -- and it doubles as
#     the escape hatch for a user who really wants the TUI gone: exceed it
#     and this script gives up, leaving the dead pane inspectable exactly
#     the way claude-supervisor.sh does.
#     (Related: Ctrl+C typed in the TUI never reaches opencode as a signal --
#     it reads the keystroke in raw mode -- so the stray-Ctrl+C failure mode
#     that motivated claude-supervisor.sh cannot happen here at all. This
#     script exists for genuine crashes, external kills and parity.)
#
#  2. "$@" is NOT replayed on restart. The operator's first invocation may
#     carry "--prompt <text>" (the only way to start the TUI with an initial
#     prompt already submitted) and/or "--fork". Replaying either would
#     re-submit the same prompt, or fork a fresh session branch, on EVERY
#     crash. Restarts use only the resumption flags derived below.
#
#  3. The resumption flag is "--continue" (opencode spells it exactly like
#     Claude Code does), or "--session <id>" when the first invocation
#     pinned a specific session -- then restarts must land in THAT session,
#     not in whatever "the last session" happens to be. "--auto" (opencode's
#     analogue of "--permission-mode auto") is carried forward when present,
#     for the same reason claude-supervisor.sh carries auto mode forward:
#     it must not silently disappear after a crash.
#
# Unlike `claude --continue`, `opencode --continue` against an EMPTY session
# store does not fail -- it just starts a fresh session -- so a first boot
# that is handed "--continue" (the operator's update/wake paths) is safe.
set -u

is_auto=0
session=""
prev=""
for arg in "$@"; do
	case "$prev" in
		--session | -s) session=$arg ;;
	esac
	case "$arg" in
		--auto) is_auto=1 ;;
		--session=*) session=${arg#--session=} ;;
		-s=*) session=${arg#-s=} ;;
	esac
	prev=$arg
done

restart_args="--continue"
if [ -n "$session" ]; then
	restart_args="--session $session"
fi
if [ "$is_auto" -eq 1 ]; then
	restart_args="--auto $restart_args"
fi

# Restart budget: identical policy to claude-supervisor.sh -- a killed
# opencode should come back instantly, but a genuinely broken one (missing
# binary, crash-looping) must not spin forever and hide the failure. Cap at
# 5 restarts per rolling 60s window; once exceeded, exit with opencode's own
# status so remain-on-exit leaves the pane inspectable.
attempt=0
max_rapid_restarts=5
window_start=$(date +%s)

opencode "$@"
status=$?

while :; do
	now=$(date +%s)
	if [ $((now - window_start)) -gt 60 ]; then
		attempt=0
		window_start=$now
	fi
	attempt=$((attempt + 1))
	if [ "$attempt" -gt "$max_rapid_restarts" ]; then
		echo "opencode-supervisor: opencode exited $status $attempt times in the last 60s -- giving up so the pane stays inspectable instead of crash-looping" >&2
		exit "$status"
	fi

	echo "opencode-supervisor: opencode exited $status (attempt $attempt/$max_rapid_restarts this minute) -- resuming the session with $restart_args in 2s" >&2
	sleep 2

	# shellcheck disable=SC2086 -- restart_args is a fixed, code-controlled
	# set of bare CLI flags plus an opencode session id (ses_...), never
	# free-form user input; left unquoted so "--auto --continue" splits into
	# two separate words.
	opencode $restart_args
	status=$?
done
