package wsbridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
)

// ActivityLogPath is the file inside an agent container that
// agent/agent-signal.sh writes to on every turn-boundary event its caller
// reports. Kept deliberately harness-agnostic, in the same spirit as
// OutputLogPath above: today only the Claude Code hooks baked into
// agent/settings.json call agent-signal.sh (UserPromptSubmit -> working,
// Stop/Notification -> waiting), but any FUTURE harness with its own
// callback mechanism can report the same two-value vocabulary by calling
// that same script -- nothing here, in internal/api, or in the frontend
// needs to know which harness wrote it. The two literals MUST stay in step.
const ActivityLogPath = "/workspace/.agent-activity"

// Activity is an agent's most recently reported turn-boundary state.
type Activity string

const (
	// ActivityWorking means the harness reported it started a turn (Claude
	// Code's UserPromptSubmit hook) and has not yet reported finishing.
	ActivityWorking Activity = "working"
	// ActivityWaiting means the harness reported the turn ended, or that it
	// otherwise needs the user's attention (Claude Code's Stop and
	// Notification hooks both report this -- a Notification also covers a
	// permission prompt or an idle-for-a-while nudge mid-turn, which is
	// exactly "waiting for the user" too).
	ActivityWaiting Activity = "waiting"
)

// ReadActivity returns the agent's last-reported Activity and when
// agent-signal.sh wrote it.
//
// ("", zero time, nil) means "unknown" rather than an error: a container
// whose harness has never wired activity signaling, or one that simply
// hasn't reported anything yet (e.g. between container start and its first
// prompt), is an ordinary state, the same stance ReadOutput takes for a
// missing output log. A malformed line (a write caught mid-rename, or a
// value outside the two known ones) degrades the same way rather than
// erroring -- a transient or unrecognised read is exactly as uninformative
// to a caller as a missing file.
func ReadActivity(ctx context.Context, docker dockerclient.ExecClient, containerID string) (Activity, time.Time, error) {
	out, code, err := runExec(ctx, docker, containerID, activityReadCmd())
	if err != nil {
		return "", time.Time{}, fmt.Errorf("reading %s from container %q: %w", ActivityLogPath, containerID, err)
	}
	if code != 0 {
		return "", time.Time{}, fmt.Errorf("reading %s from container %q: exit code %d: %s",
			ActivityLogPath, containerID, code, strings.TrimSpace(string(out)))
	}

	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return "", time.Time{}, nil
	}
	status := Activity(fields[0])
	if status != ActivityWorking && status != ActivityWaiting {
		return "", time.Time{}, nil
	}
	sec, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return "", time.Time{}, nil
	}
	return status, time.Unix(sec, 0), nil
}

// activityReadCmd builds the argv ReadActivity execs. Same "test for the
// file first" shape as readCmd above, and for the same reason: a container
// whose harness has not written ActivityLogPath yet must read back as
// empty, not as a `cat`-on-missing-file error.
func activityReadCmd() []string {
	return []string{"sh", "-c", "if [ ! -f '" + ActivityLogPath + "' ]; then exit 0; fi; exec cat '" + ActivityLogPath + "' 2>&1"}
}
