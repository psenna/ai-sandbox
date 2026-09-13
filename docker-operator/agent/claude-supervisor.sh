#!/bin/sh
# claude-supervisor.sh -- runs inside tmux-boot.sh's session in place of a
# bare `claude "$@"`.
#
# Why: tmux-boot.sh's `remain-on-exit` keeps the SESSION (and container)
# alive when `claude` dies, but it does not bring `claude` itself back -- the
# web UI terminal is left staring at a dead-but-readable pane until someone
# notices and manually respawns it. The common way `claude` dies is not a
# real crash: an accidental Ctrl+C reaching the foreground process (a stray
# keystroke, a browser tab sending it while unfocused, ...) sends SIGINT and
# ends the process. This script turns that into a transparent auto-resume
# instead of a dead terminal.
#
# A CLEAN exit (status 0 -- a deliberate `/exit`, or `claude -p` finishing
# its one-shot run) is left alone: this script does not resurrect a session
# the user asked to end. Only a NON-ZERO exit is treated as "unintentional"
# and restarted.
#
# "$@" are exactly the args tmux-boot.sh forwards on the FIRST invocation
# (create.go's autoModeArgs: nothing, or "--permission-mode auto", followed
# by a resumption flag -- "--continue" for Update, "--resume" for the
# reconcile pass's wake-up, or neither for a fresh create). Every restart
# after the first drops any resumption flag from that original set and uses
# a bare "--continue" instead -- there is now always a local, in-container
# conversation to continue -- while keeping "--permission-mode auto" if it
# was present, so auto mode doesn't silently disappear after a crash.
set -u

is_auto=0
case " $* " in
	*" --permission-mode auto "*) is_auto=1 ;;
esac

restart_args="--continue"
if [ "$is_auto" -eq 1 ]; then
	restart_args="--permission-mode auto --continue"
fi

# Restart budget: an accidental Ctrl+C should resume instantly, but a truly
# broken `claude` (missing binary, crash-looping) must not spin forever --
# that would starve the container and hide the failure. Cap at 5 restarts
# per rolling 60s window; once exceeded, exit non-zero so remain-on-exit
# takes over and leaves the pane inspectable, same as before this script
# existed.
attempt=0
max_rapid_restarts=5
window_start=$(date +%s)

claude "$@"
status=$?

while [ "$status" -ne 0 ]; do
	now=$(date +%s)
	if [ $((now - window_start)) -gt 60 ]; then
		attempt=0
		window_start=$now
	fi
	attempt=$((attempt + 1))
	if [ "$attempt" -gt "$max_rapid_restarts" ]; then
		echo "claude-supervisor: claude exited $status $attempt times in the last 60s -- giving up so the pane stays inspectable instead of crash-looping" >&2
		exit "$status"
	fi

	echo "claude-supervisor: claude exited $status (attempt $attempt/$max_rapid_restarts this minute) -- resuming the session in 2s" >&2
	sleep 2

	# shellcheck disable=SC2086 -- restart_args is a fixed, code-controlled
	# set of bare CLI flags (never user input); left unquoted so
	# "--permission-mode auto --continue" splits into three separate words.
	claude $restart_args
	status=$?
done

echo "claude-supervisor: claude exited 0 (deliberate); not restarting" >&2
