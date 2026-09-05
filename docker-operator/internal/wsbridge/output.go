package wsbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
)

// OutputLogPath is the file inside an agent container that accumulates
// everything its tmux pane writes.
//
// agent/tmux-boot.sh points `tmux pipe-pane` at this exact path immediately
// after creating the session; the two literals MUST stay in step. It lives on
// the agent's own workspace volume, so it outlives the tmux server, the
// container and the operator process -- unlike tmux's scrollback, which is
// bounded and dies with the server.
const OutputLogPath = "/workspace/.agent-output.log"

// TailAll is the tailLines value asking ReadOutput for the WHOLE log.
//
// It is deliberately the zero value: GET /api/agents/{id}/output?tail=N
// parses a missing or empty tail parameter to 0, and "no parameter means
// everything" is the only reading that makes an unparameterised request
// useful. Negative values mean the same thing rather than erroring -- there
// is no sensible "minus three lines", and rejecting them would push a
// pointless error path into every caller.
const TailAll = 0

const (
	// execTimeout bounds how long ReadOutput waits for its `tail` to finish
	// after its output has been drained. Reading a file is near-instant; this
	// only exists so a wedged daemon cannot hang a request forever. A caller
	// whose ctx has a shorter deadline still wins -- awaitExec honours both.
	execTimeout = 30 * time.Second
	// execPollInterval is how often awaitExec re-inspects the exec. Docker
	// offers no "wait for exec" call, so polling is the only option.
	execPollInterval = 50 * time.Millisecond
)

// ReadOutput returns the tail of an agent container's captured pane output.
//
// tailLines > 0 returns the last tailLines lines; TailAll (0) or any negative
// value returns the whole log. The bytes are returned exactly as the pane
// produced them: raw terminal output, CRLF line endings and ANSI escape
// sequences included. Rendering or stripping them is the caller's business,
// which is why this returns []byte and not string -- a pane's output is not
// guaranteed to be valid UTF-8, and converting here would quietly bless it as
// text.
//
// A container whose log does not exist yet -- an agent that has only just
// started, or one whose pane died before pipe-pane could attach -- yields
// (nil, nil), not an error. "No output captured yet" is an ordinary state of
// a healthy agent, and an endpoint that 500s on it would be wrong.
//
// Errors from a missing or stopped container are wrapped, not translated, so
// dockerclient.IsNotFound still recognises them and internal/api can map them
// to 404.
func ReadOutput(ctx context.Context, docker dockerclient.ExecClient, containerID string, tailLines int) ([]byte, error) {
	out, code, err := runExec(ctx, docker, containerID, readCmd(tailLines))
	if err != nil {
		return nil, fmt.Errorf("reading %s from container %q: %w", OutputLogPath, containerID, err)
	}
	if code != 0 {
		return nil, fmt.Errorf("reading %s from container %q: exit code %d: %s",
			OutputLogPath, containerID, code, strings.TrimSpace(string(out)))
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// readCmd builds the argv ReadOutput execs.
//
// It is a `sh -c` script rather than a bare `tail` for two reasons:
//
//   - The existence test. A fresh agent has no capture log until its pane
//     first writes, and `tail` on a missing file exits non-zero. Testing for
//     the file in-container turns "not there yet" into a clean empty result
//     without string-matching busybox's error message from out here.
//   - The 2>&1. VERIFIED against Docker 27.5.1: because
//     dockerclient.Docker.ExecAttach starts every exec with Tty:true, the
//     daemon hands back a single raw stream carrying ONLY stdout -- an exec's
//     stderr is silently discarded. Without this redirect a failing `tail`
//     would surface as a bare exit code with no diagnosis at all. On success
//     neither `tail` nor `cat` writes to stderr, so the payload stays clean.
//
// The path is a package constant with no shell metacharacters, and tailLines
// is rendered by strconv, so nothing here is attacker-influenced; the quoting
// is hygiene, not a control.
func readCmd(tailLines int) []string {
	read := "cat '" + OutputLogPath + "'"
	if tailLines > 0 {
		read = "tail -n " + strconv.Itoa(tailLines) + " '" + OutputLogPath + "'"
	}
	return []string{"sh", "-c", "if [ ! -f '" + OutputLogPath + "' ]; then exit 0; fi; exec " + read + " 2>&1"}
}

// runExec runs one short command in a container and returns its output and
// exit code.
//
// This is wsbridge's own copy of the idiom internal/agent's Manager.runExec
// implements, deliberately NOT a reuse of it: agent is the higher-level
// orchestrator and wsbridge is its sibling, so importing it here would invert
// the layering. dockerclient.ExecClient is the contract both share. If a
// third package ever needs this, the right home is dockerclient itself.
//
// The exec only STARTS on attach and only finishes once its output has been
// drained, so both steps are mandatory even when the output is not wanted.
func runExec(ctx context.Context, docker dockerclient.ExecClient, containerID string, cmd []string) ([]byte, int, error) {
	execID, err := docker.ExecCreate(ctx, containerID, dockerclient.ExecSpec{Cmd: cmd})
	if err != nil {
		return nil, 0, fmt.Errorf("preparing %v: %w", cmd, err)
	}
	stream, err := docker.ExecAttach(ctx, execID)
	if err != nil {
		return nil, 0, fmt.Errorf("attaching to %v: %w", cmd, err)
	}
	raw, readErr := io.ReadAll(stream)
	_ = stream.Close()
	if readErr != nil {
		return nil, 0, fmt.Errorf("reading the output of %v: %w", cmd, readErr)
	}

	status, err := awaitExec(ctx, docker, execID)
	if err != nil {
		return nil, 0, err
	}
	return decodeExecOutput(raw), status.ExitCode, nil
}

// awaitExec polls until the exec process has finished, which is the only way
// Docker offers to learn its exit status.
func awaitExec(ctx context.Context, docker dockerclient.ExecClient, execID string) (dockerclient.ExecStatus, error) {
	deadline := time.Now().Add(execTimeout)
	for {
		st, err := docker.ExecInspect(ctx, execID)
		if err != nil {
			return dockerclient.ExecStatus{}, fmt.Errorf("inspecting exec %q: %w", execID, err)
		}
		if !st.Running {
			return st, nil
		}
		if time.Now().After(deadline) {
			return dockerclient.ExecStatus{}, fmt.Errorf("exec %q did not finish within %s", execID, execTimeout)
		}
		if err := sleepCtx(ctx, execPollInterval); err != nil {
			return dockerclient.ExecStatus{}, err
		}
	}
}

// decodeExecOutput renders an exec's captured bytes as the process's own
// output.
//
// Which framing arrives is decided by the Tty flag on the exec START request,
// not by the exec's creation-time TTY -- verified against Docker 27.5.1 by
// driving /exec/{id}/start directly with both values. dockerclient's
// ExecAttach hardcodes Tty:true, so today the stream is always RAW and this
// function is a pass-through. That is a property of a neighbouring package,
// though, and #72's terminal bridge is likely to revisit exactly that call,
// so the multiplexed framing is handled rather than assumed away: the
// alternative failure mode is 8-byte binary headers silently embedded in an
// agent's output.
func decodeExecOutput(raw []byte) []byte {
	if !isMultiplexed(raw) {
		return raw
	}
	var stdout, stderr bytes.Buffer
	if err := dockerclient.DemuxStream(&stdout, &stderr, bytes.NewReader(raw)); err != nil {
		return raw
	}
	return append(stdout.Bytes(), stderr.Bytes()...)
}

// isMultiplexed reports whether raw is EXACTLY a sequence of well-formed
// Docker stream frames: an 8-byte header (stream id 0/1/2, three zero bytes,
// a big-endian payload length) followed by that many payload bytes, repeated
// until the buffer ends on a frame boundary.
//
// The check is structural and total, rather than "try to demultiplex and see
// whether it errors", because stdcopy accepts truncated input as a clean EOF
// and would turn a raw pane capture that happened to begin with a plausible
// header into silent data loss. Requiring the walk to consume the whole
// buffer makes a false positive need terminal output that starts with a NUL,
// SOH or STX followed by three NULs -- which no terminal produces.
func isMultiplexed(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	for i := 0; i < len(raw); {
		if len(raw)-i < 8 {
			return false
		}
		h := raw[i : i+8]
		if h[0] > 2 || h[1] != 0 || h[2] != 0 || h[3] != 0 {
			return false
		}
		i += 8 + int(binary.BigEndian.Uint32(h[4:8]))
		if i > len(raw) {
			return false
		}
	}
	return true
}

// sleepCtx sleeps for d, or returns early with ctx's error if it is cancelled
// first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
