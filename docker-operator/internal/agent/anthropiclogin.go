package agent

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
)

// AnthropicLoginContainerName is the fixed name of the singleton helper
// container that runs `claude setup-token` for the web UI's "Log in with
// your Claude subscription" flow. Singleton: only one login is ever in
// progress, and the name is what makes StartAnthropicLogin idempotent and
// the terminal route (GET /ws/anthropic/login/terminal) able to find it
// without a record.
const AnthropicLoginContainerName = "docker-operator-anthropic-login"

// RoleAnthropicLogin is LabelRole's value on the login helper container. It
// carries LabelManaged but no agent-id: it belongs to no agent, and the
// reconcile pass reports it (never auto-deletes it) like any other managed
// infra it does not own -- the operator tears it down itself (on
// PUT /api/anthropic/auth, DELETE /api/anthropic/login, an idle timeout, or
// at startup).
const RoleAnthropicLogin Role = "anthropic-login"

// labelLoginStartedAt records the login container's start time as a Unix
// timestamp, so the idle-timeout janitor can age it out without a clock of
// its own or a dockerclient.Container.Created field.
const labelLoginStartedAt = "ai-sandbox.docker-operator/login-started-at"

// AnthropicLoginIdleTimeout is how long a login helper may sit before the
// janitor (cmd/docker-operator) tears it down -- generous room to complete a
// browser OAuth round-trip, short enough that a walked-away session does not
// linger.
const AnthropicLoginIdleTimeout = 20 * time.Minute

// anthropicLoginScript is the login container's foreground process, passed
// as an inline Entrypoint so the agent image's own /entrypoint.sh (which
// requires AGENT_TOKEN and sets up git-proxy/DependaProxy, none of which
// `claude setup-token` needs) is skipped entirely.
//
// Same "the tmux session owns the container" shape as
// docker-operator/agent/tmux-boot.sh (see its header for the tmux 3.7c
// gotchas): remain-on-exit is set in the SAME command list as new-session so
// a pane that dies immediately still leaves a readable, dead pane rather
// than taking the container down; the pane command is quoted so the `;`
// runs it through the shell; the has-session poll is the loop condition so
// its eventual non-zero exit ends the loop cleanly under `set -eu`.
const anthropicLoginScript = `set -eu
export TERM="${TERM:-xterm-256color}"
tmux set-option -g remain-on-exit on \; new-session -d -s main 'claude setup-token; echo; echo "[login helper] copy the token above into the web UI, then remove this session"'
while tmux has-session -t main 2>/dev/null; do sleep 5; done`

// StartAnthropicLogin ensures the singleton `claude setup-token` helper
// container is running, pulling the agent image first if needed. Idempotent:
// if the container already exists it is left as-is and nil is returned.
func (m *Manager) StartAnthropicLogin(ctx context.Context) error {
	switch _, err := m.docker.ContainerInspect(ctx, AnthropicLoginContainerName); {
	case err == nil:
		return nil // already running
	case !dockerclient.IsNotFound(err):
		return fmt.Errorf("checking for an existing Anthropic-login container: %w", err)
	}

	if err := m.ensureImage(ctx, m.cfg.AgentImage); err != nil {
		return err
	}

	id, err := m.docker.ContainerCreate(ctx, dockerclient.ContainerSpec{
		Name:       AnthropicLoginContainerName,
		Image:      m.cfg.AgentImage,
		Entrypoint: []string{"sh", "-c", anthropicLoginScript},
		Env:        map[string]string{"TERM": "xterm-256color"},
		Labels: map[string]string{
			LabelManaged:        LabelManagedValue,
			LabelRole:           string(RoleAnthropicLogin),
			labelLoginStartedAt: strconv.FormatInt(time.Now().Unix(), 10),
		},
		// proxynet has plain NAT egress (it is how ollama and git-proxy
		// reach the internet), which is all `claude setup-token`'s OAuth
		// round-trip to console.anthropic.com needs. No dinernet, no DinD.
		Networks:  []dockerclient.NetworkAttachment{{Name: m.cfg.ProxynetName}},
		TTY:       true,
		OpenStdin: true,
	})
	if err != nil {
		return fmt.Errorf("creating the Anthropic-login container: %w", err)
	}
	if err := m.docker.ContainerStart(ctx, id); err != nil {
		_ = m.docker.ContainerRemove(context.WithoutCancel(ctx), id)
		return fmt.Errorf("starting the Anthropic-login container: %w", err)
	}
	return nil
}

// StopAnthropicLogin removes the login helper container. Idempotent: a
// missing container is success.
func (m *Manager) StopAnthropicLogin(ctx context.Context) error {
	if err := m.docker.ContainerStop(ctx, AnthropicLoginContainerName, m.opts.StopTimeout); err != nil && !dockerclient.IsNotFound(err) {
		return fmt.Errorf("stopping the Anthropic-login container: %w", err)
	}
	if err := m.docker.ContainerRemove(ctx, AnthropicLoginContainerName); err != nil && !dockerclient.IsNotFound(err) {
		return fmt.Errorf("removing the Anthropic-login container: %w", err)
	}
	return nil
}

// AnthropicLoginActive reports whether the login helper container currently
// exists (running or not -- a finished `claude setup-token` leaves a dead
// but still-attachable pane).
func (m *Manager) AnthropicLoginActive(ctx context.Context) (bool, error) {
	switch _, err := m.docker.ContainerInspect(ctx, AnthropicLoginContainerName); {
	case err == nil:
		return true, nil
	case dockerclient.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("inspecting the Anthropic-login container: %w", err)
	}
}

// ReapStaleAnthropicLogin tears the login helper down if it has been up
// longer than AnthropicLoginIdleTimeout. Called on a timer by
// cmd/docker-operator; a no-op when there is no login container or it is
// still young. A container whose start-time label is missing or unparseable
// is treated as stale (it predates this code, or something wrote it wrong).
func (m *Manager) ReapStaleAnthropicLogin(ctx context.Context) error {
	c, err := m.docker.ContainerInspect(ctx, AnthropicLoginContainerName)
	if dockerclient.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting the Anthropic-login container: %w", err)
	}

	fresh := false
	if raw := c.Labels[labelLoginStartedAt]; raw != "" {
		if unix, perr := strconv.ParseInt(raw, 10, 64); perr == nil {
			fresh = time.Since(time.Unix(unix, 0)) < AnthropicLoginIdleTimeout
		}
	}
	if fresh {
		return nil
	}
	m.log.InfoContext(ctx, "tearing down a stale Anthropic-login container", "name", AnthropicLoginContainerName)
	return m.StopAnthropicLogin(ctx)
}
