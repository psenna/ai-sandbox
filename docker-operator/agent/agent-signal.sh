#!/bin/sh
# agent-signal.sh <working|waiting> -- writes this agent's turn-boundary
# activity signal to a small state file on its own workspace volume, so the
# operator can tell whether the harness driving this agent is actively
# working on a turn or is idle/waiting for the user (internal/wsbridge's
# ReadActivity, behind GET /api/agents' "activity" field).
#
# Deliberately a standalone script, not inlined into settings.json's hook
# commands: today only the Claude Code hooks baked into this image's
# settings.json call it (UserPromptSubmit -> working, Stop and Notification
# -> waiting -- a Notification also covers a permission prompt or an
# idle-for-a-while nudge mid-turn, which is exactly "waiting for the user"
# too). A FUTURE harness with its own callback mechanism can report the same
# two-value vocabulary by calling this same script -- nothing on the
# operator side (Go, the API, the frontend) needs to know which harness
# called it.
set -eu

status="${1:-}"
case "$status" in
	working|waiting) ;;
	*)
		echo "agent-signal: usage: agent-signal.sh working|waiting" >&2
		exit 1
		;;
esac

# On /workspace (the agent's own durable volume), alongside the tmux output
# capture -- internal/wsbridge.ActivityLogPath must match this literal.
STATE_FILE=/workspace/.agent-activity

# Write to a temp file and rename into place, so a concurrent reader (a
# plain `cat`, in internal/wsbridge.ReadActivity) never observes a
# half-written line.
printf '%s %s\n' "$status" "$(date +%s)" > "$STATE_FILE.tmp"
mv "$STATE_FILE.tmp" "$STATE_FILE"
