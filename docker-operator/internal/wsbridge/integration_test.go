package wsbridge

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
)

// tmuxBootPath is where agent/Dockerfile bakes tmux-boot.sh. It duplicates
// internal/agent's unexported constant of the same name on purpose: these two
// packages are siblings, and exporting a constant from internal/agent purely
// so a wsbridge TEST could read it would widen the orchestrator's API for no
// production reason. The literal is pinned by agent/Dockerfile's COPY, so a
// move would break internal/agent's own tests first and loudly.
const tmuxBootPath = "/usr/local/bin/tmux-boot.sh"

// tmuxSession is the session name tmux-boot.sh creates; same reasoning.
const tmuxSession = "main"

// paneMarker is the text the stand-in pane command prints. Long and unique
// enough that it cannot appear in the image's own boot chatter.
const paneMarker = "wsbridge-itest-line"

// shimBoot is the container Cmd these tests use in place of a bare
// tmuxBootPath.
//
// It runs the REAL tmux-boot.sh -- that is the whole point, the pipe-pane
// line under test is in there -- but shadows `claude` on PATH first, so the
// pane runs something whose output is known. The real `claude` is an
// interactive TUI that needs credentials and whose stdout is not predictable
// or even guaranteed to exist; asserting on it would make this test a
// weathervane for Claude Code's rendering.
//
// The stand-in loops forever rather than printing once. tmux-boot.sh opens
// the pipe AFTER creating the session, so anything the pane writes in the
// first few milliseconds is genuinely not captured -- a one-shot echo would
// race. Looping also keeps the pane alive, which is what a healthy agent
// looks like.
const shimBoot = `set -e
mkdir -p /tmp/shim
cat > /tmp/shim/claude <<'SHIM'
#!/bin/sh
i=0
while :; do
	i=$((i + 1))
	echo "` + paneMarker + `-$i"
	sleep 0.2
done
SHIM
chmod +x /tmp/shim/claude
PATH=/tmp/shim:$PATH
export PATH
exec ` + tmuxBootPath + `
`

// deadShimBoot is shimBoot with a `claude` that exits immediately, which is
// how a missing or instantly-crashing agent binary looks to tmux-boot.sh.
const deadShimBoot = `set -e
mkdir -p /tmp/shim
printf '#!/bin/sh\nexit 3\n' > /tmp/shim/claude
chmod +x /tmp/shim/claude
PATH=/tmp/shim:$PATH
export PATH
exec ` + tmuxBootPath + `
`

// realDockerClient builds a dockerclient.Client against the daemon named by
// the standard Docker environment variables and SKIPS the calling test --
// never t.Fatal -- when none is reachable. Same factory and same skip
// discipline as internal/agent's integration tests.
func realDockerClient(t *testing.T) dockerclient.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("real docker daemon integration tests skipped: -short")
	}
	c, err := dockerclient.NewFromEnv()
	if err != nil {
		t.Skipf("building a docker client from the environment: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("no reachable docker daemon: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// agentImage returns the configured agent image, skipping the test when the
// daemon does not hold it. Building it is `make agent-image`.
func agentImage(t *testing.T, c dockerclient.Client) string {
	t.Helper()
	cfg, err := config.Load(nil, os.Getenv)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := c.ImageInspect(context.Background(), cfg.AgentImage); dockerclient.IsNotFound(err) {
		t.Skipf("agent image %q is not present on this daemon; build it first (docker-operator/agent/Dockerfile, `make agent-image`): %v", cfg.AgentImage, err)
	} else if err != nil {
		t.Fatalf("ImageInspect(%q): %v", cfg.AgentImage, err)
	}
	return cfg.AgentImage
}

// runAgentContainer creates and starts one throwaway agent container with the
// given Cmd, registering its own cleanup.
func runAgentContainer(t *testing.T, c dockerclient.Client, prefix string, cmd []string) string {
	t.Helper()
	ctx := context.Background()
	name := "docker-operator-itest-" + prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	id, err := c.ContainerCreate(ctx, dockerclient.ContainerSpec{
		Name:  name,
		Image: agentImage(t, c),
		Cmd:   cmd,
		Env: map[string]string{
			// entrypoint.sh refuses to start without these two.
			"AGENT_TOKEN": "itest-token",
			"GITHUB_REPO": "psenna/ai-sandbox.git",
			"TERM":        "xterm-256color",
		},
		TTY:       true,
		OpenStdin: true,
	})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		_ = c.ContainerStop(context.Background(), id, 5*time.Second)
		_ = c.ContainerRemove(context.Background(), id)
	})
	if err := c.ContainerStart(ctx, id); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	return id
}

// waitTmuxSession polls until tmux-boot.sh's session answers.
func waitTmuxSession(t *testing.T, c dockerclient.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, code, err := runExec(context.Background(), c, id, []string{"tmux", "has-session", "-t", tmuxSession})
		if err == nil && code == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tmux %q session never appeared in container %s within 60s (last check: exit=%d err=%v)", tmuxSession, id, code, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// paneField reads one tmux format field for the session's pane.
func paneField(t *testing.T, c dockerclient.Client, id, format string) string {
	t.Helper()
	out, code, err := runExec(context.Background(), c, id, []string{"tmux", "list-panes", "-t", tmuxSession, "-F", format})
	if err != nil || code != 0 {
		t.Fatalf("tmux list-panes -F %q: exit=%d err=%v out=%q", format, code, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestIntegrationReadOutput is #70's acceptance criterion, for real: known
// content written to the pane is retrievable through ReadOutput, with no
// terminal WebSocket ever connected (this test never opens one -- the only
// channel it uses is the exec ReadOutput itself runs).
func TestIntegrationReadOutput(t *testing.T) {
	c := realDockerClient(t)
	ctx := context.Background()
	id := runAgentContainer(t, c, "output", []string{"sh", "-c", shimBoot})
	waitTmuxSession(t, c, id)

	// The pipe must actually be open -- this is what tmux-boot.sh's new
	// pipe-pane line buys, and pane_pipe is tmux's own report of it.
	if got := paneField(t, c, id, "#{pane_pipe}"); got != "1" {
		t.Fatalf("pane_pipe = %q, want %q (tmux-boot.sh's pipe-pane did not attach)", got, "1")
	}

	// Poll: the capture only starts once pipe-pane has attached, so the very
	// first lines the pane prints may legitimately be missing.
	var all []byte
	deadline := time.Now().Add(30 * time.Second)
	for {
		var err error
		all, err = ReadOutput(ctx, c, id, TailAll)
		if err != nil {
			t.Fatalf("ReadOutput(TailAll): %v", err)
		}
		if bytes.Count(all, []byte(paneMarker)) >= 10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d %q lines were captured in %s: %q", bytes.Count(all, []byte(paneMarker)), paneMarker, OutputLogPath, all)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("captured %d bytes of pane output, %d %q lines", len(all), bytes.Count(all, []byte(paneMarker)), paneMarker)

	// The capture is raw terminal output: a tmux pane always writes CRLF.
	if !bytes.Contains(all, []byte("\r\n")) {
		t.Errorf("captured output has no CRLF line endings, which a tmux pane always produces: %q", all)
	}

	// tail=N really tails.
	const want = 5
	tail, err := ReadOutput(ctx, c, id, want)
	if err != nil {
		t.Fatalf("ReadOutput(%d): %v", want, err)
	}
	if got := bytes.Count(tail, []byte("\n")); got != want {
		t.Errorf("ReadOutput(%d) returned %d lines, want %d: %q", want, got, want, tail)
	}
	if len(tail) >= len(all) {
		t.Errorf("ReadOutput(%d) returned %d bytes, ReadOutput(TailAll) returned %d; the tail must be the shorter of the two", want, len(tail), len(all))
	}
	for _, line := range bytes.Split(bytes.TrimRight(tail, "\r\n"), []byte("\n")) {
		if !bytes.Contains(line, []byte(paneMarker)) {
			t.Errorf("tail line %q does not carry the marker %q", line, paneMarker)
		}
	}

	t.Logf("PASS: %s captured the pane's output and ReadOutput read it back over a plain exec, with no terminal WebSocket involved", OutputLogPath)
}

// TestIntegrationReadOutputNoCapture covers the honest empty case end to end:
// a container running the agent image with no tmux session and therefore no
// capture log at all must read back as empty, not as an error. This is the
// state internal/api will see for an agent that has only just been created.
func TestIntegrationReadOutputNoCapture(t *testing.T) {
	c := realDockerClient(t)
	id := runAgentContainer(t, c, "output-empty", []string{"sh", "-c", "sleep 300"})

	got, err := ReadOutput(context.Background(), c, id, TailAll)
	if err != nil {
		t.Fatalf("ReadOutput on a container with no capture log = %v, want no error", err)
	}
	if got != nil {
		t.Errorf("ReadOutput = %q, want nil when %s does not exist", got, OutputLogPath)
	}
}

// TestIntegrationPipePaneNeverKillsTheContainer guards the regression the
// pipe-pane line could so easily have introduced.
//
// tmux pipe-pane exits 1 with "target pane has exited" when the pane's
// process is already gone -- exactly the case #69's remain-on-exit exists to
// keep inspectable. Under tmux-boot.sh's `set -e` an UNGUARDED pipe-pane
// therefore ends the script, stops the container and destroys the evidence.
// Confirmed by building both variants: without the `||` guard this same
// container exits 1 within a second.
func TestIntegrationPipePaneNeverKillsTheContainer(t *testing.T) {
	c := realDockerClient(t)
	ctx := context.Background()
	id := runAgentContainer(t, c, "output-dead", []string{"sh", "-c", deadShimBoot})

	// Long enough for tmux-boot.sh to reach and fail its pipe-pane call.
	time.Sleep(8 * time.Second)

	cont, err := c.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if cont.State != dockerclient.StateRunning {
		t.Fatalf("container State = %q (exit %d) after its pane died before pipe-pane could attach, want %q: a failed pipe-pane must never take the agent down",
			cont.State, cont.ExitCode, dockerclient.StateRunning)
	}
	if _, code, err := runExec(ctx, c, id, []string{"tmux", "has-session", "-t", tmuxSession}); err != nil || code != 0 {
		t.Errorf("tmux has-session: exit=%d err=%v, want exit 0 (remain-on-exit keeps the dead pane inspectable)", code, err)
	}
	if got := paneField(t, c, id, "#{pane_dead}"); got != "1" {
		t.Errorf("pane_dead = %q, want %q", got, "1")
	}

	// And the read-back stays honest rather than erroring: pipe-pane never
	// attached, so there is no log.
	out, err := ReadOutput(ctx, c, id, TailAll)
	if err != nil {
		t.Errorf("ReadOutput after a failed pipe-pane = %v, want no error", err)
	}
	if out != nil {
		t.Errorf("ReadOutput = %q, want nil", out)
	}

	t.Logf("PASS: a pipe-pane that failed on an already-dead pane left container %s %s with an inspectable dead pane", id, cont.State)
}
